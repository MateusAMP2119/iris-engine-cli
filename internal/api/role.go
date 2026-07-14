package api

import (
	"net/http"
	"sync"
)

// This file holds the daemon's leadership role as the HTTP surface sees it: the
// role a healthz probe reports and the gate a standby uses to reject mutations.
// Only the leader writes meta; a standby (or a candidate not yet confirmed
// leader) rejects mutating requests with guidance pointing at the leader, while
// reads work on any role ("reads work anywhere"). The daemon owns a RoleState,
// flips it during election, and hands it to the mux, which consults it per
// request -- so api never reaches up into the daemon.

// Role is the daemon's leadership role as reported on the wire.
type Role string

// The leadership roles.
const (
	// RoleUnknown is the role before election resolves (or when no reporter is
	// wired). A daemon of unknown role rejects mutations: only a confirmed leader
	// writes meta.
	RoleUnknown Role = "unknown"
	// RoleLeader is the elected leader: it holds the advisory lock and is the sole
	// meta writer, so it accepts mutations.
	RoleLeader Role = "leader"
	// RoleStandby is a candidate blocked on the leader lock: it serves reads and
	// rejects mutations with leader guidance.
	RoleStandby Role = "standby"
)

// The not_leader control-plane rejection: the error a standby returns for a
// mutation, with the leader hint the CLI turns into exit-6 guidance. The code is a
// control-plane code (not one of the read-API closed set), deliberately distinct so
// a client can tell "you asked a standby to mutate" from an ordinary error; E11
// pins the final wire shape.
const (
	// CodeNotLeader is the machine code a standby returns for a rejected mutation.
	CodeNotLeader = "not_leader"
	// StatusNotLeader is the HTTP status for a not_leader rejection: 421 Misdirected
	// Request -- the mutation was directed at a node (a standby) that cannot produce
	// the response; retry it against the leader.
	StatusNotLeader = http.StatusMisdirectedRequest
	// leaderUnknown is the leader hint when no leader address is known yet.
	leaderUnknown = "unknown"
)

// RoleReporter is the leadership role the mux consults per request: the current
// role and, for a standby, a hint at where the leader is. The daemon's RoleState
// implements it; the mux depends only on this interface.
type RoleReporter interface {
	// Role returns the daemon's current leadership role.
	Role() Role
	// LeaderHint returns the current leader's address for standby guidance, or ""
	// when no leader is known.
	LeaderHint() string
}

// RoleState is the daemon's live leadership role, safe for concurrent reads
// (per-request, from listener goroutines) and writes (from the election goroutine).
// The zero value is not usable; build one with NewRoleState.
type RoleState struct {
	mu     sync.RWMutex
	role   Role
	leader string
}

// compile-time proof RoleState is a RoleReporter the mux can consult.
var _ RoleReporter = (*RoleState)(nil)

// NewRoleState returns a role state in the unknown role: before election resolves,
// the daemon is neither confirmed leader nor standby, and it rejects mutations.
func NewRoleState() *RoleState {
	return &RoleState{role: RoleUnknown}
}

// SetLeader marks this daemon the elected leader: it accepts mutations and reports
// no leader hint (it is the leader).
func (s *RoleState) SetLeader() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.role = RoleLeader
	s.leader = ""
}

// SetStandby marks this daemon a standby, recording the leader address (may be ""
// when unknown) for the guidance a rejected mutation returns.
func (s *RoleState) SetStandby(leaderHint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.role = RoleStandby
	s.leader = leaderHint
}

// Role returns the current leadership role.
func (s *RoleState) Role() Role {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.role
}

// LeaderHint returns the recorded leader address, or "" when none is known.
func (s *RoleState) LeaderHint() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.leader
}

// unknownRole is the default reporter when no RoleState is wired: it always reports
// the unknown role, so a mux built with no role still rejects mutations (only a
// confirmed leader writes).
type unknownRole struct{}

func (unknownRole) Role() Role         { return RoleUnknown }
func (unknownRole) LeaderHint() string { return "" }

// isSafeMethod reports whether an HTTP method is a read (safe) method. Safe methods
// pass the standby gate on any role; every other method is a mutation, gated to the
// leader.
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// WriteNotLeader writes the standby's mutation rejection: the not_leader error
// envelope with the leader hint, at StatusNotLeader. A missing hint renders as
// "unknown" so the client always gets a well-formed leader field.
func WriteNotLeader(w http.ResponseWriter, leaderHint string) {
	if leaderHint == "" {
		leaderHint = leaderUnknown
	}
	writeJSON(w, StatusNotLeader, notLeaderEnvelope{
		Error: notLeaderBody{
			Code:    CodeNotLeader,
			Message: "this daemon is not the leader; retry the mutation against the leader",
			Leader:  leaderHint,
		},
	})
}

// notLeaderEnvelope is the control-plane not_leader document: the error envelope
// plus the leader hint the standby points the client at.
type notLeaderEnvelope struct {
	Error notLeaderBody `json:"error"`
}

// notLeaderBody is the error object inside a notLeaderEnvelope.
type notLeaderBody struct {
	// Code is the not_leader machine code.
	Code string `json:"code"`
	// Message is the human-readable guidance.
	Message string `json:"message"`
	// Leader is the leader's address for the client to retry against, or "unknown".
	Leader string `json:"leader"`
}
