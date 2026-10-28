import { useCallback, useState } from "react";
import { Link } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Checkbox } from "../components/Field";
import { Alert, EmptyState, Loading, PageHeader } from "../components/Feedback";
import { useLoader } from "../hooks";

type List = Schemas["NotificationList"];

/** Links in messages are absolute when the server knows its external URL; keep navigation inside the app. */
function inAppLink(link: string): string {
  try {
    const u = new URL(link, window.location.origin);
    return u.pathname;
  } catch {
    return "/";
  }
}

export function NotificationsPage() {
  const [list, setList] = useState<List>();
  const [error, setError] = useState<string>();
  const [unreadOnly, setUnreadOnly] = useState(false);
  const load = useCallback(async () => {
    const { data, error } = await api.GET("/notifications", {
      params: { query: { unread: unreadOnly || undefined, limit: 100 } },
    });
    if (data) setList(data);
    else setError(describeError(error));
  }, [unreadOnly]);
  useLoader(load);

  async function markRead(id?: string) {
    const { error } = await api.POST("/notifications/read", {
      body: id ? { id } : {},
    });
    if (error) setError(describeError(error));
    else await load();
  }

  if (!list && !error) return <Loading />;
  return (
    <>
      <PageHeader
        title="Notifications"
        description="Builds you started, checks and publications for them, and approvals waiting for you."
        actions={
          list && list.unread > 0 ? (
            <Button variant="secondary" onPress={() => void markRead()}>
              Mark all as read
            </Button>
          ) : undefined
        }
      />
      {error && <Alert>{error}</Alert>}
      <Checkbox isSelected={unreadOnly} onChange={setUnreadOnly}>
        Unread only
      </Checkbox>
      {list && list.items.length === 0 && (
        <EmptyState title="Nothing new">
          Notifications appear here when something you are involved in changes.
        </EmptyState>
      )}
      {list && list.items.length > 0 && (
        <ul className="samuori-inbox" aria-label="Notifications">
          {list.items.map((n) => (
            <li key={n.id} className={n.readAt ? "" : "samuori-inbox-unread"}>
              <div>
                <Link href={inAppLink(n.message.link)}>
                  <strong>{n.message.title}</strong>
                </Link>
                {n.message.text && <p>{n.message.text}</p>}
                <span className="samuori-muted">
                  {new Date(n.message.occurredAt).toLocaleString()}
                </span>
              </div>
              {!n.readAt && (
                <Button variant="quiet" onPress={() => void markRead(n.id)}>
                  Mark as read
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
    </>
  );
}
