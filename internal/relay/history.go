package relay

// recentIDs bounds memory used to recognize late messages. Callers hold their
// request/stream lock. IDs older than this horizon are rejected as unknown.
const recentIDLimit = 4096

type recentIDs struct {
	ids   map[string]struct{}
	order []string
	next  int
}

func (history *recentIDs) add(id string) {
	if history.contains(id) {
		return
	}
	if history.ids == nil {
		history.ids = make(map[string]struct{})
	}
	if len(history.order) < recentIDLimit {
		history.order = append(history.order, id)
	} else {
		delete(history.ids, history.order[history.next])
		history.order[history.next] = id
		history.next = (history.next + 1) % recentIDLimit
	}
	history.ids[id] = struct{}{}
}

func (history *recentIDs) contains(id string) bool {
	_, found := history.ids[id]
	return found
}
