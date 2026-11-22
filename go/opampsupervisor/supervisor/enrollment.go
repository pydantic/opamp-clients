// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/pydantic/opamp-clients/go/opamp"
	"github.com/pydantic/opamp-clients/go/opamp/collector"
)

// A server that enrolls hosts answers one that comes with an enrollment token with a credential
// of its own, and closes the connection; the host connects with the credential next, and that is
// when the server takes it, or refuses it. A host refused there goes back to its token, which the
// server answers with another credential, at once and every time: without an end to it the host
// takes credentials it never uses, one a second, until the server says it has too many. The core
// makes the connections and keeps the first credential (the server may have taken the host
// without it hearing so), so the Supervisor ends the loop: it stops the client, says why in the
// status, and starts it again later, with the saved credential first.

// How long the client is held for, variables for the tests: holdTaken when the server said this
// host's identity is another installation's, which waiting won't change soon; otherwise, for what
// may pass, first holdClosed when it ended the connections made with its credentials and holdLost
// when they were lost, doubling each time to holdTaken. An endpoint it offered that can't be reached is
// tried again as after a close, with the same credential: no other is taken meanwhile.
var (
	holdTaken  = time.Hour
	holdClosed = time.Minute
	holdLost   = 5 * time.Second
)

// roundsUnserved is how many times the server may offer the host a credential and close its
// connection, with no try with one seen to fail, before the client is held (as after a close).
const roundsUnserved = 3

// EnrollmentState is how a host that the server hasn't taken stands with it.
type EnrollmentState struct {
	// Offers is how many credentials the server offered this host in this run, none of them in
	// use; Since is when it was first not taken.
	Offers int       `json:"offers"`
	Since  time.Time `json:"since"`
	// Error is why, as the server or the connection said: "" while nothing went wrong yet.
	// IdentityTaken: the server said this host's identity is bound to another installation.
	Error         string `json:"error,omitempty"`
	IdentityTaken bool   `json:"identity_taken,omitempty"`
	// Unreachable is the host (and port) of the endpoint that the server offered this host a credential
	// for, if it can't be reached: the credential was never tried.
	Unreachable string `json:"unreachable,omitempty"`
	// TokenRefused: the server closes the connection made with the enrollment token unanswered.
	TokenRefused bool `json:"token_refused,omitempty"`
	// RetryAt is when the host next tries, if it waits: the server asked it to, or the Supervisor
	// stopped trying for now. Zero otherwise.
	RetryAt time.Time `json:"retry_at,omitzero"`
}

// waiting is whether the host waits before it tries again.
func (e *EnrollmentState) waiting(now time.Time) bool { return e != nil && e.RetryAt.After(now) }

// print writes the Accepted line of a host not taken.
func (e *EnrollmentState) print(line func(label, format string, args ...any), in func(time.Time) string, now time.Time) {
	offers := ""
	switch {
	case e.Offers == 1:
		offers = "the server offered this host a credential of its own, not in use yet"
	case e.Offers > 1:
		offers = fmt.Sprintf("the server offered this host %d credentials of its own, none of them in use", e.Offers)
	}
	switch {
	case e.Error == "":
		line("Accepted", "not yet: %s", offers)
	case offers == "":
		line("Accepted", "no: %s", e.Error)
	default:
		line("Accepted", "no: %s", offers)
		line("", "%s", e.Error)
	}
	if e.IdentityTaken {
		line("", "The server holds this host's identity for another installation (a host it deleted, say): remove this host's data and enroll it again")
	}
	if e.Unreachable != "" {
		line("", "This host must reach %s (its name, a firewall, a proxy, its certificate), where the server told it to connect", e.Unreachable)
		line("", "If the server was wrong to: once it is corrected there, remove this host's data and enroll it again")
	}
	if e.TokenRefused {
		line("", "The enrollment key may be used up, expired or revoked, or the server may not take this host: check both in the server's UI")
	}
	if e.waiting(now) {
		line("", "Not trying again before %s; restart the Supervisor to try now", in(e.RetryAt))
	}
}

// enrollment is the client's: with coreClient.mu held.
type enrollment struct {
	// held is, while the client is held (stopped, to be started again), closed once that ends.
	held chan struct{}
	// holds counts the holds in a row; offers is the credentials offered before the last one.
	holds  int
	offers int
	// unreached is the host of the endpoint offered that couldn't be reached, at the last hold.
	unreached string
	// uid is the instance UID in use when the client was held.
	uid [16]byte
}

// carryUnreached takes up from the status the last run left that the endpoint its server offered this host a
// credential for couldn't be reached: a restart takes no other either. With c.mu held.
func (c *coreClient) carryUnreached(endpoint string) {
	last := c.last
	c.last = nil
	if last == nil || last.Endpoint != endpoint || last.Enrollment == nil || last.Enrollment.Unreachable == "" {
		return
	}
	e := *last.Enrollment
	e.RetryAt = time.Time{}
	c.enroll.holds, c.enroll.offers, c.enroll.unreached = 1, e.Offers, e.Unreachable
	c.report.mu.Lock()
	defer c.report.mu.Unlock()
	c.report.st.Enrollment = &e
}

// holding is whether the client is held.
func (c *coreClient) holding() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enroll.held != nil
}

// ifCurrent calls report if cl is the client that runs, and says whether it is: one held may still
// tell something, over what the status says of it.
func (c *coreClient) ifCurrent(cl *collector.Client, report func()) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != cl {
		return false
	}
	report()
	return true
}

// released ends the hold, with c.mu held.
func (c *coreClient) released() {
	close(c.enroll.held)
	c.enroll.held = nil
}

// watchStanding keeps the status, and holds the client, by how it stands with its server, until
// quit is closed or the client is held.
func (c *coreClient) watchStanding(cl *collector.Client, quit <-chan struct{}) {
	for {
		select {
		case <-cl.StandingChanged():
			if c.onStanding(cl, cl.Standing(), quit) {
				return
			}
		case <-quit:
			return
		}
	}
}

// onStanding is whether the client is held for it.
func (c *coreClient) onStanding(cl *collector.Client, st opamp.Standing, quit <-chan struct{}) bool {
	c.mu.Lock()
	if c.client != cl {
		c.mu.Unlock()
		return true
	}
	if st.Accepted {
		c.enroll.holds, c.enroll.offers, c.enroll.unreached = 0, 0, ""
		c.mu.Unlock()
		c.report.accepted(c.logger, st.Takes)
		return false
	}
	state := enrollmentOf(st, c.enroll.offers)
	wait := holdFor(st, c.enroll.holds, time.Now())
	if wait == 0 && !st.ClaimClosed && st.Offers == 0 && !state.TokenRefused {
		state.Unreachable = c.enroll.unreached // until something else is known
	}
	if wait == 0 {
		c.mu.Unlock()
		if state == (EnrollmentState{}) {
			c.report.noEnrollment(c.logger)
		} else {
			c.report.enrollment(c.logger, state)
		}
		return false
	}
	if state.Error == "" {
		state.Error = "the server offers credentials and never takes the host with one"
	}
	state.RetryAt = time.Now().Add(wait)
	c.enroll.held = make(chan struct{})
	c.enroll.holds++
	c.enroll.offers, c.enroll.unreached = state.Offers, state.Unreachable
	c.enroll.uid = cl.InstanceUID()
	c.client, c.opened = nil, nil
	c.report.enrollmentHeld(c.logger, state)
	c.mu.Unlock()

	msg := "The OpAMP server doesn't take this host with the credentials it offers it: not trying again for now"
	if state.Unreachable != "" {
		msg = "The endpoint the OpAMP server offers this host a credential for can't be reached: not trying again for now"
	}
	c.logger.Error(msg,
		zap.Int("credentials_offered", state.Offers), zap.String("reason", state.Error), zap.Time("retry_at", state.RetryAt))
	go c.hold(cl, wait, quit)
	return true
}

// enrollmentOf is what the status says of a host that stands so with its server, offered so many
// credentials before.
func enrollmentOf(st opamp.Standing, offeredBefore int) EnrollmentState {
	state := EnrollmentState{Offers: offeredBefore + st.Offers, IdentityTaken: st.IdentityTaken}
	// A credential from before this run that fails, with nothing offered since, is the connection's
	// to tell (Server): the server may only be down.
	if st.ClaimError != "" && (st.Offers > 0 || st.IdentityTaken) {
		state.Error = "connecting with the offered credential failed: " + st.ClaimError
		if st.ClaimClosed {
			state.Error = "the server didn't take the host with the offered credential: " + st.ClaimError
		}
		if st.IdentityTaken {
			state.Error = "the server says this host's identity is already bound to another installation: " + st.ClaimError
		}
	}
	if st.Unreached != "" {
		state.Unreachable = st.Unreached
		state.Error = fmt.Sprintf("the server offers the credential for %s, which can't be reached: %s", st.Unreached, st.ClaimError)
	}
	if st.Refused != "" && state.Error == "" {
		state.TokenRefused = true
		state.Error = "the server doesn't take the credentials this host is configured with: " + st.Refused
	}
	if st.Dropped != "" && state.Error == "" {
		state.Error = "the server took this host, and then dropped it: " + st.Dropped
	}
	if st.Error != "" {
		// What the server says now; what failed before is what it comes of.
		if state.Error != "" {
			state.Error = "; " + state.Error
		}
		state.Error = "the server said: " + st.Error + state.Error
		state.RetryAt = st.RetryAt
	}
	return state
}

// holdFor is how long the client is held for, standing so with its server after so many holds in a
// row: 0 not at all.
func holdFor(st opamp.Standing, holds int, now time.Time) time.Duration {
	switch {
	case st.Accepted, st.RetryAt.After(now): // the core waits, as the server asked
		return 0
	case st.Unreached != "":
		return backedOff(holdClosed, holds)
	case !st.OfferedAgain && st.Rounds < roundsUnserved && (st.Offers < 2 || st.FailedClaims < 2):
		return 0
	case st.IdentityTaken:
		return holdTaken
	case st.ClaimError != "" && !st.ClaimClosed:
		return backedOff(holdLost, holds)
	}
	return backedOff(holdClosed, holds)
}

// backedOff is first, doubled holds times, to holdTaken at most.
func backedOff(first time.Duration, holds int) time.Duration {
	return min(first<<min(holds, 16), holdTaken)
}

// hold stops cl, waits, and starts the client again as it was started.
func (c *coreClient) hold(cl *collector.Client, wait time.Duration, quit <-chan struct{}) {
	// Bounded by the client's own stop timeout. The state file is its alone until it has stopped.
	if err := cl.Stop(context.Background()); err != nil {
		c.logger.Debug("The OpAMP client stopped with an error", zap.Error(err))
	}
	for {
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-quit:
			timer.Stop()
			c.mu.Lock()
			defer c.mu.Unlock()
			c.released()
			return
		}
		err := c.resume()
		if err == nil {
			return
		}
		// The state file can't be read for now, say.
		wait = backedOff(holdLost, 2)
		c.logger.Error("The OpAMP client can't start again: trying again soon", zap.Duration("in", wait), zap.Error(err))
	}
}

// resume starts the client again as Start did, with what was set since; nil too once it is
// stopped for good.
func (c *coreClient) resume() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		c.released()
		return nil
	}
	// The instance UID it started with: one assigned or made since is in the state file.
	settings := c.settings
	if err := c.openLocked(context.Background(), settings); err != nil {
		return err
	}
	if err := c.startLocked(context.Background()); err != nil {
		c.opened.Close()
		c.opened = nil
		return err
	}
	c.released()
	return nil
}

// enrollment records how the host's enrollment goes.
func (r *statusReport) enrollment(logger *zap.Logger, e EnrollmentState) {
	r.update(logger, func(st *Status, now time.Time) {
		e.Since = now
		if st.Enrollment != nil {
			e.Since = st.Enrollment.Since
		}
		st.Enrollment, st.Accepted = &e, nil
	})
}

// noEnrollment records that the host isn't taken, and nothing is known against it (a server that says
// nothing keeps its connection open, say).
func (r *statusReport) noEnrollment(logger *zap.Logger) {
	r.update(logger, func(st *Status, _ time.Time) { st.Enrollment, st.Accepted = nil, nil })
}

// enrollmentHeld records that the Supervisor stopped trying for now.
func (r *statusReport) enrollmentHeld(logger *zap.Logger, e EnrollmentState) {
	r.enrollment(logger, e)
	r.update(logger, func(st *Status, now time.Time) {
		why := "the server doesn't take this host"
		if e.Unreachable != "" {
			why = "the endpoint the server offers can't be reached"
		}
		st.Server = &ServerStatus{Since: now, Error: "not connecting for now: " + why + " (enrollment)"}
	})
}
