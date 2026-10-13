package browser

import (
	"fmt"
	"strconv"
	"strings"
)

// refRegistry maps model-facing refs ("e12") to Chrome backend DOM node ids.
// A ref is stable for as long as its node lives: snapshots of the same
// document reuse it. Numbering never restarts within a session, so a ref
// from an earlier document can never silently name a node on a later one.
type refRegistry struct {
	document  string
	next      int
	byBackend map[int]string
	byRef     map[string]int
}

func newRefRegistry() *refRegistry {
	return &refRegistry{byBackend: map[int]string{}, byRef: map[string]int{}}
}

// observe forgets every ref when the main frame has loaded a new document.
func (r *refRegistry) observe(document string) {
	if document == r.document {
		return
	}
	r.document = document
	clear(r.byBackend)
	clear(r.byRef)
}

func (r *refRegistry) ref(backendID int) string {
	if ref, ok := r.byBackend[backendID]; ok {
		return ref
	}
	r.next++
	ref := "e" + strconv.Itoa(r.next)
	r.byBackend[backendID] = ref
	r.byRef[ref] = backendID
	return ref
}

// lookup resolves ref, explaining why an unresolvable ref failed.
func (r *refRegistry) lookup(ref string) (int, error) {
	ref = strings.TrimSpace(ref)
	if id, ok := r.byRef[ref]; ok {
		return id, nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(ref, "e"))
	if !strings.HasPrefix(ref, "e") || err != nil || n < 1 {
		return 0, fmt.Errorf("ref %q is not a snapshot ref (refs look like e12)", ref)
	}
	if n <= r.next {
		return 0, staleRefError(ref)
	}
	return 0, fmt.Errorf("ref %q was never issued; take a snapshot first", ref)
}

type staleRefError string

func (e staleRefError) Error() string {
	return fmt.Sprintf("ref %s is stale: the element is gone or the page changed; take a new snapshot", string(e))
}
