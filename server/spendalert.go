package server

import (
	"context"
	"fmt"
	"time"

	"github.com/hanzoai/base/core"
	"github.com/hanzoai/base/tools/types"
	gen "github.com/hanzoai/platform-services/gen"
)

// SpendAlert is the ONLY category whose data lives in Base. It is the native
// port of the console spendAlertRouter's `cloudSpendAlert` Prisma table: per-org
// spend thresholds with full CRUD, every row scoped to the caller's org. All
// other categories proxy upstream; this one owns its rows.
//
// Validation matches the router's zod schema exactly, enforced at the boundary:
//   - title: 1..100 chars
//   - threshold: > 0 and <= 1_000_000 (USD)
// Org ownership is enforced on update/delete (a row from another org is NOT
// FOUND, never mutated).

const spendAlertMaxThreshold = 1_000_000.0

// handleSpendAlert dispatches the spendAlert CRUD ops against the Base
// collection. org is the authoritative scope from the verified capability.
func (s *Server) handleSpendAlert(_ context.Context, org string, req psRequest) (uint32, []byte) {
	switch req.Op {
	case OpSpendAlertList:
		return s.spendAlertList(org)
	case OpSpendAlertCreate:
		return s.spendAlertCreate(org, req)
	case OpSpendAlertUpdate:
		return s.spendAlertUpdate(org, req)
	case OpSpendAlertDelete:
		return s.spendAlertDelete(org, req)
	default:
		return fail(StatusBadRequest, fmt.Sprintf("spendAlert: unknown op %d", req.Op))
	}
}

// spendAlertList returns the org's alerts, newest first (created desc).
func (s *Server) spendAlertList(org string) (uint32, []byte) {
	col, err := s.app.FindCollectionByNameOrId(SpendAlertCollection)
	if err != nil {
		return fail(StatusInternal, "spendAlert: collection missing: "+err.Error())
	}
	recs, err := s.app.FindRecordsByFilter(col, "org = {:org}", "-created", 0, 0, map[string]any{"org": org})
	if err != nil {
		return fail(StatusInternal, "spendAlert.list: "+err.Error())
	}
	views := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		views = append(views, spendAlertJSON(r))
	}
	return respond(StatusOK, views)
}

// spendAlertCreate validates + inserts a new alert for the org.
func (s *Server) spendAlertCreate(org string, req psRequest) (uint32, []byte) {
	var p struct {
		Title     string  `json:"title"`
		Threshold float64 `json:"threshold"`
	}
	if !req.bind(&p) {
		return fail(StatusBadRequest, "spendAlert.create: malformed params")
	}
	if status, msg := validateAlert(p.Title, p.Threshold); status != StatusOK {
		return fail(status, msg)
	}
	col, err := s.app.FindCollectionByNameOrId(SpendAlertCollection)
	if err != nil {
		return fail(StatusInternal, "spendAlert: collection missing: "+err.Error())
	}
	rec := core.NewRecord(col)
	rec.Set(faOrg, org)
	rec.Set(faTitle, p.Title)
	rec.Set(faThreshold, p.Threshold)
	if err := s.app.Save(rec); err != nil {
		return fail(StatusInternal, "spendAlert.create: "+err.Error())
	}
	return respond(StatusOK, spendAlertJSON(rec))
}

// spendAlertUpdate mutates an existing alert, asserting org ownership first.
func (s *Server) spendAlertUpdate(org string, req psRequest) (uint32, []byte) {
	var p struct {
		ID        string   `json:"id"`
		Title     *string  `json:"title"`
		Threshold *float64 `json:"threshold"`
	}
	if !req.bind(&p) || p.ID == "" {
		return fail(StatusBadRequest, "spendAlert.update: id required")
	}
	rec, status, msg := s.spendAlertOwned(org, p.ID)
	if status != StatusOK {
		return fail(status, msg)
	}
	if p.Title != nil {
		if st, m := validateTitle(*p.Title); st != StatusOK {
			return fail(st, m)
		}
		rec.Set(faTitle, *p.Title)
	}
	if p.Threshold != nil {
		if st, m := validateThreshold(*p.Threshold); st != StatusOK {
			return fail(st, m)
		}
		rec.Set(faThreshold, *p.Threshold)
	}
	if err := s.app.Save(rec); err != nil {
		return fail(StatusInternal, "spendAlert.update: "+err.Error())
	}
	return respond(StatusOK, spendAlertJSON(rec))
}

// spendAlertDelete removes an alert, asserting org ownership first.
func (s *Server) spendAlertDelete(org string, req psRequest) (uint32, []byte) {
	var p struct {
		ID string `json:"id"`
	}
	if !req.bind(&p) || p.ID == "" {
		return fail(StatusBadRequest, "spendAlert.delete: id required")
	}
	rec, status, msg := s.spendAlertOwned(org, p.ID)
	if status != StatusOK {
		return fail(status, msg)
	}
	if err := s.app.Delete(rec); err != nil {
		return fail(StatusInternal, "spendAlert.delete: "+err.Error())
	}
	return respond(StatusOK, map[string]bool{"success": true})
}

// spendAlertOwned loads an alert by id and verifies it belongs to org. A row
// owned by another org is reported NOT FOUND (never leaked, never mutated) —
// exactly the router's behaviour.
func (s *Server) spendAlertOwned(org, id string) (*core.Record, uint32, string) {
	rec, err := s.app.FindRecordById(SpendAlertCollection, id)
	if err != nil {
		return nil, StatusNotFound, "spendAlert: not found"
	}
	if rec.GetString(faOrg) != org {
		return nil, StatusNotFound, "spendAlert: not found"
	}
	return rec, StatusOK, ""
}

// --- validation (mirrors the router's zod schema) ---

func validateAlert(title string, threshold float64) (uint32, string) {
	if status, msg := validateTitle(title); status != StatusOK {
		return status, msg
	}
	return validateThreshold(threshold)
}

func validateTitle(title string) (uint32, string) {
	if n := len([]rune(title)); n < 1 || n > 100 {
		return StatusBadRequest, "spendAlert: title must be 1..100 chars"
	}
	return StatusOK, ""
}

func validateThreshold(threshold float64) (uint32, string) {
	if threshold <= 0 || threshold > spendAlertMaxThreshold {
		return StatusBadRequest, "spendAlert: threshold must be > 0 and <= 1,000,000"
	}
	return StatusOK, ""
}

// --- record <-> view marshaling ---

// spendAlertJSON renders a record as the JSON object the console expects.
func spendAlertJSON(r *core.Record) map[string]any {
	return map[string]any{
		"id":          r.Id,
		"orgId":       r.GetString(faOrg),
		"title":       r.GetString(faTitle),
		"threshold":   r.GetFloat(faThreshold),
		"triggeredAt": dtOrNil(r.GetDateTime(faTriggeredAt)),
		"createdAt":   dtString(r.GetDateTime(faCreated)),
		"updatedAt":   dtString(r.GetDateTime(faUpdated)),
	}
}

// SpendAlertBuf renders a record as a typed SpendAlert ZAP sub-buffer. Exposed
// for callers (and the SpendAlertList typed view) that want zero-copy access to
// the stored entity rather than the JSON projection.
func SpendAlertBuf(r *core.Record) []byte {
	return gen.NewSpendAlert(gen.SpendAlertInput{
		Id:          r.Id,
		Org:         r.GetString(faOrg),
		Title:       r.GetString(faTitle),
		Threshold:   r.GetFloat(faThreshold),
		TriggeredAt: dtUnix(r.GetDateTime(faTriggeredAt)),
		Created:     dtUnix(r.GetDateTime(faCreated)),
		Updated:     dtUnix(r.GetDateTime(faUpdated)),
	})
}

// dtUnix returns the unix-second value of a Base DateTime, or 0 if zero.
func dtUnix(dt types.DateTime) int64 {
	if dt.IsZero() {
		return 0
	}
	return dt.Time().Unix()
}

// dtString returns an RFC3339 UTC string for a DateTime ("" if zero).
func dtString(dt types.DateTime) string {
	if dt.IsZero() {
		return ""
	}
	return dt.Time().UTC().Format(time.RFC3339)
}

// dtOrNil returns an RFC3339 string for a non-zero DateTime, else nil (the
// router emitted null for a never-triggered alert).
func dtOrNil(dt types.DateTime) any {
	if dt.IsZero() {
		return nil
	}
	return dt.Time().UTC().Format(time.RFC3339)
}
