package api

import (
	"net/http"
	"slices"

	"samuori.invalid/samuori/internal/api/apigen"
)

// pageOf pages a list that is read whole, keeping its order: the cursor is the key of the last item already
// returned. An unknown cursor answers 400 invalid_cursor rather than silently restarting or skipping. Without a limit
// or cursor it returns every item, as these lists did before they were paginated.
func pageOf[T any](w http.ResponseWriter, items []T, limit *int, cursor *string, key func(T) string) ([]T, *string, bool) {
	if limit == nil && cursor == nil {
		return items, nil, true
	}
	l, c := pageParams(limit, cursor)
	if c != "" {
		i := slices.IndexFunc(items, func(x T) bool { return key(x) == c })
		if i < 0 {
			apigen.WriteProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is not valid; list again without it")
			return nil, nil, false
		}
		items = items[i+1:]
	}
	if len(items) <= l {
		return items, nil, true
	}
	next := key(items[l-1])
	return items[:l], &next, true
}

// pageLimited trims a list fetched with limit+1 rows and returns the cursor for the next page.
func pageLimited[T any](items []T, limit int, key func(T) string) ([]T, *string) {
	if len(items) <= limit {
		return items, nil
	}
	items = items[:limit]
	next := key(items[limit-1])
	return items, &next
}
