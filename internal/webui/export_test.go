package webui

import (
	"github.com/shukiv/gniza/internal/panel"
	"os"

	"github.com/shukiv/gniza/internal/granular"
)

// PrepareSocketDirForTest exposes the directory rule to the test package.
func PrepareSocketDirForTest(dir string, mode os.FileMode) error {
	return prepareSocketDir(dir, mode)
}

// InFlightForTest exposes the per-account limit.
type InFlightForTest struct{ f inFlight }

func (i *InFlightForTest) Enter(key string) bool { return i.f.enter(key) }
func (i *InFlightForTest) Leave(key string)      { i.f.leave(key) }

// ConnectionBudgetForTest exposes the pre-HTTP socket budget.
type ConnectionBudgetForTest struct{ b connectionBudget }

func NewConnectionBudgetForTest(total, perUID int) *ConnectionBudgetForTest {
	return &ConnectionBudgetForTest{b: connectionBudget{
		maxTotal: total, maxPerUID: perUID, byUID: map[uint32]int{},
	}}
}

func (b *ConnectionBudgetForTest) Acquire(uid uint32) bool { return b.b.acquire(uid) }
func (b *ConnectionBudgetForTest) Release(uid uint32)      { b.b.release(uid) }

// CSRF separation is a trust-boundary property: account-facing pages must
// never reveal the token used by root's WHM interface.
func (s *Server) AdminCSRFForTest() string              { return s.csrfToken }
func (s *Server) UserCSRFForTest(account string) string { return s.userCSRFToken(account) }

// IsUserKindForTest exposes what an account is allowed to ask for.
func IsUserKindForTest(kind string) bool { return isUserKind(granular.Kind(kind)) }

// AccountFromCredentialsForTest exposes the admission check behind the
// account-facing socket, with the panel's word supplied by the test.
func AccountFromCredentialsForTest(holder panel.AccountHolder, uid uint32) (string, error) {
	return accountFromCredentials(holder, uid)
}
