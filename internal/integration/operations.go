// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goabonga/maestro/internal/state"
)

// Type is the kind of a journaled operation.
type Type string

// The types of operation.
const (
	// Integrate builds one aggregated commit from a task's work.
	Integrate Type = "INTEGRATE"
	// Sync brings the integration branch to an imported commit.
	Sync Type = "SYNC"
	// Publish publishes already tested and committed operations.
	Publish Type = "PUBLISH"
)

// State is the journaled state of an operation.
type State string

// The states of an operation. COMMITTED and ROLLED_BACK are terminal.
const (
	Prepared   State = "PREPARED"
	Started    State = "STARTED"
	Applied    State = "APPLIED"
	Tested     State = "TESTED"
	Committed  State = "COMMITTED"
	Failed     State = "FAILED"
	RolledBack State = "ROLLED_BACK"
)

// Terminal reports whether no transition leaves the state.
func (s State) Terminal() bool {
	return s == Committed || s == RolledBack
}

// transitions is the single authority on which state an operation of
// each type may move to from which state. INTEGRATE and SYNC go through
// TESTED; PUBLISH commits straight from APPLIED, its evidence being the
// operations it publishes. Every non-terminal state may fail, and only
// a failed operation rolls back.
var transitions = map[Type]map[State][]State{
	Integrate: {
		Prepared: {Started, Failed},
		Started:  {Applied, Failed},
		Applied:  {Tested, Failed},
		Tested:   {Committed, Failed},
		Failed:   {RolledBack},
	},
	Sync: {
		Prepared: {Started, Failed},
		Started:  {Applied, Failed},
		Applied:  {Tested, Failed},
		Tested:   {Committed, Failed},
		Failed:   {RolledBack},
	},
	Publish: {
		Prepared: {Started, Failed},
		Started:  {Applied, Failed},
		Applied:  {Committed, Failed},
		Failed:   {RolledBack},
	},
}

// Allowed reports whether the transition table lets an operation of
// type t move from one state to another.
func Allowed(t Type, from, to State) bool {
	for _, next := range transitions[t][from] {
		if next == to {
			return true
		}
	}
	return false
}

// Errors of the operations journal.
var (
	// ErrInvalid reports an operation missing an input or carrying a
	// malformed one.
	ErrInvalid = errors.New("invalid operation")
	// ErrTransition reports a transition the table refuses in the
	// operation's state, or one lost to a concurrent transition.
	ErrTransition = errors.New("operation transition refused")
	// ErrGuard reports an allowed transition whose evidence is missing.
	ErrGuard = errors.New("operation guard not satisfied")
	// ErrNotFound reports an unknown operation.
	ErrNotFound = errors.New("unknown operation")
)

// Signature is one frozen identity of a commit: a name, an email and
// an instant with its time-zone offset, to the second.
type Signature struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	When  time.Time `json:"when"`
}

// CommitMetadata freezes, before a commit is built, every input of the
// commit besides its tree and parent, so the commit can be rebuilt with
// the same identity.
type CommitMetadata struct {
	Message   string    `json:"message"`
	Author    Signature `json:"author"`
	Committer Signature `json:"committer"`
}

// Operation is one journaled INTEGRATE, SYNC or PUBLISH operation.
type Operation struct {
	ID                    string
	Type                  Type
	State                 State
	Version               int64
	ProjectID             string
	ConfigID              string
	TaskID                string
	WorkerID              string
	AttemptID             string
	SupersedesOperationID string
	TaskBaseSHA           string
	IntegrationBaseSHA    string
	SourceHeadSHA         string
	// SourceCommits is the ordered commit chain, oldest first.
	SourceCommits       []string
	CandidateRef        string
	CandidateTreeSHA    string
	ResultSHA           string
	CommitMetadata      *CommitMetadata
	TestReportIDs       []string
	ReviewArtifactIDs   []string
	ApprovalArtifactIDs []string
	Error               string
	StartedAt           time.Time
	UpdatedAt           time.Time
}

// Evidence is the proof attached to an operation when it is tested or
// committed: test reports, reviews and approvals, by artifact id.
type Evidence struct {
	TestReportIDs       []string
	ReviewArtifactIDs   []string
	ApprovalArtifactIDs []string
}

// Record is one recorded event of an operation.
type Record struct {
	ID          int64
	OperationID string
	Event       string
	From        State
	To          State
	Reason      string
	At          time.Time
}

// The events of the journal.
const (
	eventPrepare   = "prepare"
	eventStart     = "start"
	eventCandidate = "record-candidate"
	eventApply     = "apply"
	eventTest      = "test"
	eventCommit    = "commit"
	eventFail      = "fail"
	eventRollBack  = "roll-back"
)

var (
	// shaID matches a full SHA-1 or SHA-256 object id.
	shaID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	// operationID matches the UUID of an operation.
	operationID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// candidateRef matches a reference name Maestro gives a candidate.
	candidateRef = regexp.MustCompile(`^refs/maestro/[A-Za-z0-9_-]+(?:/[A-Za-z0-9_-]+)*$`)
)

// resultRefPrefix is the namespace of the durable operation references,
// which a candidate reference never enters.
const resultRefPrefix = "refs/maestro/operations/"

// Store persists operations and their events.
type Store struct {
	DB *state.DB
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// now reads the store's clock in UTC.
func (s Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Prepare journals a new operation in PREPARED with every input needed
// to build its result, before any Git mutation. The caller fills the
// type, project, configuration snapshot, worker, attempt and the frozen
// Git inputs: an integration also names its task, task base, source
// head, ordered source commits ending at that head, and its commit
// metadata. The result fields are filled later by the transitions. The
// message of the metadata is stored ending with exactly one newline.
func (s Store) Prepare(op Operation) (Operation, error) {
	if err := validatePrepare(&op); err != nil {
		return Operation{}, err
	}
	if op.SupersedesOperationID != "" {
		previous, err := s.Get(op.SupersedesOperationID)
		if err != nil {
			return Operation{}, err
		}
		if previous.ProjectID != op.ProjectID || previous.TaskID != op.TaskID || previous.Type != op.Type {
			return Operation{}, fmt.Errorf("%w: operation %s belongs to another project, task or type", ErrInvalid, previous.ID)
		}
		if previous.State == Committed {
			return Operation{}, fmt.Errorf("%w: operation %s is committed", ErrInvalid, previous.ID)
		}
	}
	at := s.now()
	op.ID, op.State, op.Version, op.StartedAt, op.UpdatedAt = newID(), Prepared, 1, at, at
	metadata, err := encodeMetadata(op.CommitMetadata)
	if err != nil {
		return Operation{}, err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return Operation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`INSERT INTO operations
		(id, type, state, version, project_id, config_id, task_id, worker_id, attempt_id, supersedes_operation_id,
		task_base_sha, integration_base_sha, source_head_sha, source_commits, commit_metadata, started_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID, string(op.Type), string(op.State), op.Version, op.ProjectID, op.ConfigID, nullable(op.TaskID),
		op.WorkerID, op.AttemptID, nullable(op.SupersedesOperationID), op.TaskBaseSHA, op.IntegrationBaseSHA,
		op.SourceHeadSHA, encodeList(op.SourceCommits), metadata, stamp(at), stamp(at))
	if err != nil {
		return Operation{}, fmt.Errorf("prepare operation: %w", err)
	}
	if err := record(tx, Record{OperationID: op.ID, Event: eventPrepare, To: Prepared, At: at}); err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Operation{}, err
	}
	return op, nil
}

// validatePrepare checks the inputs of a new operation and normalizes
// its commit message.
func validatePrepare(op *Operation) error {
	if _, ok := transitions[op.Type]; !ok {
		return fmt.Errorf("%w: unknown type %q", ErrInvalid, op.Type)
	}
	for name, value := range map[string]string{
		"project": op.ProjectID, "configuration snapshot": op.ConfigID,
		"worker": op.WorkerID, "attempt": op.AttemptID,
	} {
		if value == "" {
			return fmt.Errorf("%w: no %s", ErrInvalid, name)
		}
	}
	if op.SupersedesOperationID != "" && !operationID.MatchString(op.SupersedesOperationID) {
		return fmt.Errorf("%w: superseded operation %q is not an operation id", ErrInvalid, op.SupersedesOperationID)
	}
	if !shaID.MatchString(op.IntegrationBaseSHA) {
		return fmt.Errorf("%w: integration base %q is not an object id", ErrInvalid, op.IntegrationBaseSHA)
	}
	for name, value := range map[string]string{"task base": op.TaskBaseSHA, "source head": op.SourceHeadSHA} {
		if value != "" && !shaID.MatchString(value) {
			return fmt.Errorf("%w: %s %q is not an object id", ErrInvalid, name, value)
		}
	}
	for _, commit := range op.SourceCommits {
		if !shaID.MatchString(commit) {
			return fmt.Errorf("%w: source commit %q is not an object id", ErrInvalid, commit)
		}
	}
	if op.State != "" || op.CandidateRef != "" || op.CandidateTreeSHA != "" || op.ResultSHA != "" ||
		len(op.TestReportIDs) > 0 || len(op.ReviewArtifactIDs) > 0 || len(op.ApprovalArtifactIDs) > 0 || op.Error != "" {
		return fmt.Errorf("%w: a new operation carries no state, candidate, result, evidence or error", ErrInvalid)
	}
	if op.Type == Integrate {
		switch {
		case op.TaskID == "":
			return fmt.Errorf("%w: an integration names its task", ErrInvalid)
		case op.TaskBaseSHA == "" || op.SourceHeadSHA == "":
			return fmt.Errorf("%w: an integration names its task base and source head", ErrInvalid)
		case len(op.SourceCommits) == 0 || op.SourceCommits[len(op.SourceCommits)-1] != op.SourceHeadSHA:
			return fmt.Errorf("%w: the source commits of an integration end at its source head", ErrInvalid)
		case op.CommitMetadata == nil:
			return fmt.Errorf("%w: an integration freezes its commit metadata", ErrInvalid)
		}
	}
	if op.CommitMetadata != nil {
		frozen, err := op.CommitMetadata.normalize()
		if err != nil {
			return err
		}
		op.CommitMetadata = &frozen
	}
	return nil
}

// normalize validates commit metadata and returns it with its message
// ending with exactly one newline.
func (m CommitMetadata) normalize() (CommitMetadata, error) {
	message := strings.TrimRight(m.Message, "\n")
	switch {
	case strings.TrimSpace(message) == "":
		return CommitMetadata{}, fmt.Errorf("%w: empty commit message", ErrInvalid)
	case !utf8.ValidString(message) || strings.ContainsRune(message, 0):
		return CommitMetadata{}, fmt.Errorf("%w: the commit message is not UTF-8 text", ErrInvalid)
	}
	for role, signature := range map[string]Signature{"author": m.Author, "committer": m.Committer} {
		if err := signature.validate(); err != nil {
			return CommitMetadata{}, fmt.Errorf("%w: %s: %w", ErrInvalid, role, err)
		}
	}
	m.Message = message + "\n"
	return m, nil
}

// validate checks that Git records the signature exactly as frozen: a
// name and an email Git would not trim or reject, and a positive
// instant to the second with an offset in whole minutes.
func (s Signature) validate() error {
	for field, value := range map[string]string{"name": s.Name, "email": s.Email} {
		if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "<>\n\x00") ||
			crud(value[0]) || crud(value[len(value)-1]) {
			return fmt.Errorf("the %s %q is not recorded verbatim by Git", field, value)
		}
	}
	if s.When.Unix() <= 0 || s.When.Nanosecond() != 0 {
		return fmt.Errorf("the date %s is not a positive instant to the second", s.When)
	}
	if _, offset := s.When.Zone(); offset%60 != 0 {
		return fmt.Errorf("the date %s has an offset Git cannot record", s.When)
	}
	return nil
}

// crud reports the bytes Git trims from both ends of a name or email.
func crud(c byte) bool {
	return c <= ' ' || strings.IndexByte(".,:;<>\"\\'", c) >= 0
}

// Start moves a prepared operation to STARTED.
func (s Store) Start(id, reason string) (Operation, error) {
	return s.transition(id, eventStart, Started, nil, reason)
}

// RecordCandidate records, on a started operation, the reference and
// the tree of its candidate before the result commit is built. The
// tree is frozen once recorded: recording the same values again is a
// no-op and a different tree is refused with ErrGuard.
func (s Store) RecordCandidate(id, ref, treeSHA string) (Operation, error) {
	if ref != "" && (!candidateRef.MatchString(ref) || strings.HasPrefix(ref, resultRefPrefix)) {
		return Operation{}, fmt.Errorf("%w: candidate reference %q", ErrInvalid, ref)
	}
	if !shaID.MatchString(treeSHA) {
		return Operation{}, fmt.Errorf("%w: candidate tree %q is not an object id", ErrInvalid, treeSHA)
	}
	current, err := s.Get(id)
	if err != nil {
		return Operation{}, err
	}
	if current.State == Started && current.CandidateRef == ref && current.CandidateTreeSHA == treeSHA {
		return current, nil
	}
	return s.transition(id, eventCandidate, Started, func(op *Operation) error {
		if op.State != Started {
			return fmt.Errorf("%w: a candidate is recorded in %s, not %s", ErrTransition, Started, op.State)
		}
		if op.CandidateTreeSHA != "" {
			return fmt.Errorf("%w: the candidate tree is frozen at %s", ErrGuard, op.CandidateTreeSHA)
		}
		op.CandidateRef, op.CandidateTreeSHA = ref, treeSHA
		return nil
	}, "candidate tree "+treeSHA)
}

// RecordResult moves a started SYNC or PUBLISH operation to APPLIED
// with its result commit. An integration result is only recorded from
// its durable result reference, by ApplyIntegration.
func (s Store) RecordResult(id, resultSHA string) (Operation, error) {
	current, err := s.Get(id)
	if err != nil {
		return Operation{}, err
	}
	if current.Type == Integrate {
		return current, fmt.Errorf("%w: an integration result is recorded from its result reference", ErrGuard)
	}
	return s.recordResult(id, resultSHA)
}

// recordResult moves a started operation to APPLIED with its result.
// A SYNC or INTEGRATE operation must have recorded its candidate tree.
func (s Store) recordResult(id, resultSHA string) (Operation, error) {
	if !shaID.MatchString(resultSHA) {
		return Operation{}, fmt.Errorf("%w: result %q is not an object id", ErrInvalid, resultSHA)
	}
	return s.transition(id, eventApply, Applied, func(op *Operation) error {
		if op.Type != Publish && op.CandidateTreeSHA == "" {
			return fmt.Errorf("%w: no candidate tree is recorded", ErrGuard)
		}
		op.ResultSHA = resultSHA
		return nil
	}, "result "+resultSHA)
}

// MarkTested moves an applied INTEGRATE or SYNC operation to TESTED
// with its evidence, which must include at least one test report.
func (s Store) MarkTested(id string, evidence Evidence) (Operation, error) {
	return s.transition(id, eventTest, Tested, func(op *Operation) error {
		if err := op.attach(evidence); err != nil {
			return err
		}
		if len(op.TestReportIDs) == 0 {
			return fmt.Errorf("%w: no test report", ErrGuard)
		}
		return nil
	}, "")
}

// Commit moves an operation to COMMITTED once its publication is
// confirmed, attaching any final evidence. A committed operation always
// carries test reports: its own, or for PUBLISH those of the operations
// it publishes. Nothing commits an operation implicitly.
func (s Store) Commit(id string, evidence Evidence) (Operation, error) {
	return s.transition(id, eventCommit, Committed, func(op *Operation) error {
		if err := op.attach(evidence); err != nil {
			return err
		}
		if len(op.TestReportIDs) == 0 {
			return fmt.Errorf("%w: no test report", ErrGuard)
		}
		return nil
	}, "")
}

// Fail moves a non-terminal operation to FAILED with its error.
func (s Store) Fail(id, cause string) (Operation, error) {
	cause = strings.TrimSpace(cause)
	if cause == "" {
		return Operation{}, fmt.Errorf("%w: a failure carries its error", ErrInvalid)
	}
	return s.transition(id, eventFail, Failed, func(op *Operation) error {
		op.Error = cause
		return nil
	}, cause)
}

// RollBack moves a failed operation to ROLLED_BACK once the rollback
// is confirmed.
func (s Store) RollBack(id, reason string) (Operation, error) {
	return s.transition(id, eventRollBack, RolledBack, nil, reason)
}

// attach adds evidence to an operation, keeping each id once.
func (op *Operation) attach(evidence Evidence) error {
	for _, ids := range [][]string{evidence.TestReportIDs, evidence.ReviewArtifactIDs, evidence.ApprovalArtifactIDs} {
		for _, id := range ids {
			if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) {
				return fmt.Errorf("%w: evidence id %q", ErrInvalid, id)
			}
		}
	}
	op.TestReportIDs = unionIDs(op.TestReportIDs, evidence.TestReportIDs)
	op.ReviewArtifactIDs = unionIDs(op.ReviewArtifactIDs, evidence.ReviewArtifactIDs)
	op.ApprovalArtifactIDs = unionIDs(op.ApprovalArtifactIDs, evidence.ApprovalArtifactIDs)
	return nil
}

// unionIDs returns a new list with the ids of a followed by those of b
// not already present.
func unionIDs(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, id := range b {
		if !hasID(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// hasID reports whether list holds id.
func hasID(list []string, id string) bool {
	for _, item := range list {
		if item == id {
			return true
		}
	}
	return false
}

// transition applies one change to an operation. Unless the change
// keeps the state, the table must allow the move. The new row is
// stored with its event in one transaction, and only if the operation
// is still at the state and version that were read: a concurrent
// transition makes this one fail with ErrTransition rather than
// overwrite it. A refused change leaves no trace.
func (s Store) transition(id, event string, to State, change func(*Operation) error, reason string) (Operation, error) {
	current, err := s.Get(id)
	if err != nil {
		return Operation{}, err
	}
	if to != current.State && !Allowed(current.Type, current.State, to) {
		return current, fmt.Errorf("%w: %s %s from %s to %s", ErrTransition, current.Type, id, current.State, to)
	}
	next := current
	if change != nil {
		if err := change(&next); err != nil {
			return current, err
		}
	}
	at := s.now()
	next.State, next.Version, next.UpdatedAt = to, current.Version+1, at

	tx, err := s.DB.Begin()
	if err != nil {
		return Operation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`UPDATE operations SET state = ?, version = ?, candidate_ref = ?, candidate_tree_sha = ?,
		result_sha = ?, test_report_ids = ?, review_artifact_ids = ?, approval_artifact_ids = ?, error = ?, updated_at = ?
		WHERE id = ? AND state = ? AND version = ?`,
		string(next.State), next.Version, next.CandidateRef, next.CandidateTreeSHA, next.ResultSHA,
		encodeList(next.TestReportIDs), encodeList(next.ReviewArtifactIDs), encodeList(next.ApprovalArtifactIDs),
		next.Error, stamp(at), id, string(current.State), current.Version)
	if err != nil {
		return Operation{}, fmt.Errorf("transition operation %s: %w", id, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return Operation{}, err
	} else if changed != 1 {
		return Operation{}, fmt.Errorf("%w: %s changed concurrently", ErrTransition, id)
	}
	if err := record(tx, Record{OperationID: id, Event: event, From: current.State, To: to, Reason: reason, At: at}); err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Operation{}, err
	}
	return next, nil
}

// columns are the stored fields of an operation, in scan order.
const columns = `id, type, state, version, project_id, config_id, COALESCE(task_id, ''), worker_id, attempt_id,
	COALESCE(supersedes_operation_id, ''), task_base_sha, integration_base_sha, source_head_sha, source_commits,
	candidate_ref, candidate_tree_sha, result_sha, commit_metadata, test_report_ids, review_artifact_ids,
	approval_artifact_ids, error, started_at, updated_at`

// scan reads one operation row selected with columns.
func scan(row interface{ Scan(...any) error }) (Operation, error) {
	var op Operation
	var kind, current, commits, metadata, tests, reviews, approvals, started, updated string
	err := row.Scan(&op.ID, &kind, &current, &op.Version, &op.ProjectID, &op.ConfigID, &op.TaskID, &op.WorkerID,
		&op.AttemptID, &op.SupersedesOperationID, &op.TaskBaseSHA, &op.IntegrationBaseSHA, &op.SourceHeadSHA, &commits,
		&op.CandidateRef, &op.CandidateTreeSHA, &op.ResultSHA, &metadata, &tests, &reviews, &approvals, &op.Error,
		&started, &updated)
	if err != nil {
		return Operation{}, err
	}
	op.Type, op.State = Type(kind), State(current)
	for target, text := range map[*[]string]string{
		&op.SourceCommits: commits, &op.TestReportIDs: tests, &op.ReviewArtifactIDs: reviews, &op.ApprovalArtifactIDs: approvals,
	} {
		if err := json.Unmarshal([]byte(text), target); err != nil {
			return Operation{}, fmt.Errorf("operation %s: %w", op.ID, err)
		}
	}
	if metadata != "" {
		op.CommitMetadata = new(CommitMetadata)
		if err := json.Unmarshal([]byte(metadata), op.CommitMetadata); err != nil {
			return Operation{}, fmt.Errorf("operation %s: %w", op.ID, err)
		}
	}
	if op.StartedAt, err = time.Parse(time.RFC3339Nano, started); err != nil {
		return Operation{}, err
	}
	if op.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return Operation{}, err
	}
	return op, nil
}

// Get returns an operation by id.
func (s Store) Get(id string) (Operation, error) {
	op, err := scan(s.DB.QueryRow(`SELECT `+columns+` FROM operations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return op, err
}

// List returns the operations of a project, oldest first.
func (s Store) List(projectID string) ([]Operation, error) {
	rows, err := s.DB.Query(`SELECT `+columns+` FROM operations WHERE project_id = ?
		ORDER BY started_at, id`, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ops []Operation
	for rows.Next() {
		op, err := scan(rows)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

// Events returns the recorded events of an operation, oldest first.
func (s Store) Events(id string) ([]Record, error) {
	rows, err := s.DB.Query(`SELECT event_id, event, from_state, to_state, reason, at
		FROM operation_events WHERE operation_id = ? ORDER BY event_id`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []Record
	for rows.Next() {
		r := Record{OperationID: id}
		var from, to, at string
		if err := rows.Scan(&r.ID, &r.Event, &from, &to, &r.Reason, &at); err != nil {
			return nil, err
		}
		r.From, r.To = State(from), State(to)
		if r.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// record appends one event inside the caller's transaction.
func record(tx *sql.Tx, r Record) error {
	_, err := tx.Exec(`INSERT INTO operation_events (operation_id, event, from_state, to_state, reason, at)
		VALUES (?, ?, ?, ?, ?, ?)`, r.OperationID, r.Event, string(r.From), string(r.To), r.Reason, stamp(r.At))
	if err != nil {
		return fmt.Errorf("record operation event: %w", err)
	}
	return nil
}

// encodeMetadata renders frozen commit metadata for storage; absent
// metadata is stored empty.
func encodeMetadata(m *CommitMetadata) (string, error) {
	if m == nil {
		return "", nil
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// encodeList renders an id list for storage.
func encodeList(ids []string) string {
	if ids == nil {
		ids = []string{}
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		panic(err) // a string slice always encodes
	}
	return string(encoded)
}

// nullable stores an empty optional reference as NULL.
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// stamp renders an instant for storage.
func stamp(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}

// newID returns a random UUID version 4.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
