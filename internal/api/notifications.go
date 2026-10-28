package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/notify"
)

func (a *API) notifyError(w http.ResponseWriter, r *http.Request, err error) {
	var inv *notify.Invalid
	var inUse *notify.InUse
	switch {
	case errors.As(err, &inUse):
		apigen.WriteProblem(w, http.StatusConflict, "channel_in_use", "Rules still send events to this channel: "+strings.Join(inUse.Rules, "; ")+". Delete those rules or move them to another channel, or disable the channel instead.")
	case errors.Is(err, notify.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators manage notifications")
	case errors.Is(err, notify.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, notify.ErrConflict):
		apigen.WriteProblem(w, http.StatusConflict, "conflict", "that name is already used")
	case errors.As(err, &inv):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", inv.Reason)
	default:
		a.internalError(w, r, err)
	}
}

type notificationView struct {
	ID        string         `json:"id"`
	Message   notify.Message `json:"message"`
	ReadAt    *time.Time     `json:"readAt,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}

func (a *API) ListNotifications(w http.ResponseWriter, r *http.Request, params apigen.ListNotificationsParams) {
	limit, _ := pageParams(params.Limit, nil)
	list, err := a.notify.Inbox(r.Context(), principal(r), params.Unread != nil && *params.Unread, limit)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	unread, err := a.notify.Inbox(r.Context(), principal(r), true, 200)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := struct {
		Items  []notificationView `json:"items"`
		Unread int                `json:"unread"`
	}{Items: []notificationView{}, Unread: len(unread)}
	for _, n := range list {
		out.Items = append(out.Items, notificationView{n.ID, n.Message, n.ReadAt, n.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) MarkNotificationsRead(w http.ResponseWriter, r *http.Request, _ apigen.MarkNotificationsReadParams) {
	var in struct {
		ID string `json:"id"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if err := a.notify.MarkRead(r.Context(), principal(r), in.ID); err != nil {
		a.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type channelView struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Kind                string    `json:"kind"`
	URL                 string    `json:"url,omitempty"`
	Recipients          []string  `json:"recipients,omitempty"`
	AllowPrivateNetwork bool      `json:"allowPrivateNetwork"`
	Enabled             bool      `json:"enabled"`
	CreatedAt           time.Time `json:"createdAt"`
	SigningKey          string    `json:"signingKey,omitempty"`
}

func toChannel(c notify.Channel) channelView {
	return channelView{ID: c.ID, Name: c.Name, Kind: c.Kind, URL: c.URL, Recipients: c.Recipients, AllowPrivateNetwork: c.AllowPrivateNetwork, Enabled: c.Enabled, CreatedAt: c.CreatedAt}
}

func (a *API) ListNotificationChannels(w http.ResponseWriter, r *http.Request) {
	list, err := a.notify.Channels(r.Context(), principal(r))
	if err != nil {
		a.notifyError(w, r, err)
		return
	}
	out := struct {
		Items []channelView `json:"items"`
	}{Items: []channelView{}}
	for _, c := range list {
		out.Items = append(out.Items, toChannel(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateNotificationChannel(w http.ResponseWriter, r *http.Request, _ apigen.CreateNotificationChannelParams) {
	var in struct {
		Name                string   `json:"name"`
		Kind                string   `json:"kind"`
		URL                 string   `json:"url"`
		Recipients          []string `json:"recipients"`
		Secret              string   `json:"secret"`
		AllowPrivateNetwork bool     `json:"allowPrivateNetwork"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	c, key, err := a.notify.CreateChannel(r.Context(), principal(r), notify.Channel{Name: in.Name, Kind: in.Kind, URL: in.URL, Recipients: in.Recipients, AllowPrivateNetwork: in.AllowPrivateNetwork}, in.Secret, requestID(w))
	if err != nil {
		a.notifyError(w, r, err)
		return
	}
	v := toChannel(c)
	v.SigningKey = key
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, v)
}

func (a *API) UpdateNotificationChannel(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.UpdateNotificationChannelParams) {
	var in struct {
		Name                string   `json:"name"`
		URL                 string   `json:"url"`
		Recipients          []string `json:"recipients"`
		Secret              string   `json:"secret"`
		AllowPrivateNetwork bool     `json:"allowPrivateNetwork"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	c, err := a.notify.UpdateChannel(r.Context(), principal(r), id, notify.ChannelChange{Name: in.Name, URL: in.URL, Recipients: in.Recipients, Secret: in.Secret, AllowPrivateNetwork: in.AllowPrivateNetwork}, requestID(w))
	if err != nil {
		a.notifyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toChannel(c))
}

func (a *API) DeleteNotificationChannel(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DeleteNotificationChannelParams) {
	if err := a.notify.DeleteChannel(r.Context(), principal(r), id, requestID(w)); err != nil {
		a.notifyError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) SetNotificationChannelState(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.SetNotificationChannelStateParams) {
	var in apigen.EnabledState
	if !decodeBody(w, r, &in) {
		return
	}
	if err := a.notify.SetEnabled(r.Context(), principal(r), id, in.Enabled, requestID(w)); err != nil {
		a.notifyError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) TestNotificationChannel(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.TestNotificationChannelParams) {
	err := a.notify.Test(r.Context(), principal(r), id, requestID(w))
	if errors.Is(err, notify.ErrForbidden) || errors.Is(err, notify.ErrNotFound) {
		a.notifyError(w, r, err)
		return
	}
	res := map[string]any{"status": "ok", "testedAt": time.Now().UTC()}
	if err != nil {
		msg := err.Error()
		if len(msg) > 1000 {
			msg = msg[:1000]
		}
		res["status"], res["detail"] = "failed", msg
	}
	writeJSON(w, http.StatusOK, res)
}

type ruleView struct {
	ID        string   `json:"id"`
	ChannelID string   `json:"channelId"`
	Events    []string `json:"events"`
	TeamID    string   `json:"teamId,omitempty"`
	ProjectID string   `json:"projectId,omitempty"`
}

func (a *API) ListNotificationRules(w http.ResponseWriter, r *http.Request) {
	list, err := a.notify.Rules(r.Context(), principal(r))
	if err != nil {
		a.notifyError(w, r, err)
		return
	}
	out := struct {
		Items []ruleView `json:"items"`
	}{Items: []ruleView{}}
	for _, x := range list {
		out.Items = append(out.Items, ruleView{x.ID, x.ChannelID, x.Events, x.TeamID, x.ProjectID})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateNotificationRule(w http.ResponseWriter, r *http.Request, _ apigen.CreateNotificationRuleParams) {
	var in struct {
		ChannelID string   `json:"channelId"`
		Events    []string `json:"events"`
		TeamID    string   `json:"teamId"`
		ProjectID string   `json:"projectId"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	x, err := a.notify.CreateRule(r.Context(), principal(r), notify.Rule{ChannelID: in.ChannelID, Events: in.Events, TeamID: in.TeamID, ProjectID: in.ProjectID}, requestID(w))
	if err != nil {
		a.notifyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ruleView{x.ID, x.ChannelID, x.Events, x.TeamID, x.ProjectID})
}

func (a *API) UpdateNotificationRule(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.UpdateNotificationRuleParams) {
	var in struct {
		ChannelID string   `json:"channelId"`
		Events    []string `json:"events"`
		TeamID    string   `json:"teamId"`
		ProjectID string   `json:"projectId"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	x, err := a.notify.UpdateRule(r.Context(), principal(r), id, notify.Rule{ChannelID: in.ChannelID, Events: in.Events, TeamID: in.TeamID, ProjectID: in.ProjectID}, requestID(w))
	if err != nil {
		a.notifyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ruleView{x.ID, x.ChannelID, x.Events, x.TeamID, x.ProjectID})
}

func (a *API) DeleteNotificationRule(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DeleteNotificationRuleParams) {
	if err := a.notify.DeleteRule(r.Context(), principal(r), id, requestID(w)); err != nil {
		a.notifyError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type deliveryView struct {
	ID            string     `json:"id"`
	ChannelID     string     `json:"channelId"`
	EventID       string     `json:"eventId"`
	EventType     string     `json:"eventType"`
	State         string     `json:"state"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `json:"nextAttemptAt,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	DeliveredAt   *time.Time `json:"deliveredAt,omitempty"`
}

func (a *API) ListNotificationDeliveries(w http.ResponseWriter, r *http.Request, params apigen.ListNotificationDeliveriesParams) {
	state := ""
	if params.State != nil {
		state = string(*params.State)
	}
	limit, _ := pageParams(params.Limit, nil)
	list, err := a.notify.Deliveries(r.Context(), principal(r), state, limit)
	if err != nil {
		a.notifyError(w, r, err)
		return
	}
	out := struct {
		Items []deliveryView `json:"items"`
	}{Items: []deliveryView{}}
	for _, d := range list {
		v := deliveryView{ID: d.ID, ChannelID: d.ChannelID, EventID: d.EventID, EventType: d.EventType, State: d.State, Attempts: d.Attempts, LastError: d.LastError, CreatedAt: d.CreatedAt, DeliveredAt: d.DeliveredAt}
		if d.State == "pending" {
			next := d.NextAttemptAt
			v.NextAttemptAt = &next
		}
		out.Items = append(out.Items, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) RetryNotificationDelivery(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.RetryNotificationDeliveryParams) {
	if err := a.notify.Retry(r.Context(), principal(r), id, requestID(w)); err != nil {
		a.notifyError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
