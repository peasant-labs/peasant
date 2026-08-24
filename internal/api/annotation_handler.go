package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// annotationHandler handles HTTP requests for the annotation REST API.
// annotationStore is used for reading/writing annotations and types.
// typeReader is used for type lookup and value validation.
// hub is optional: when non-nil, annotation creates broadcast updates
// to WebSocket subscribers on the annotations channel.
type annotationHandler struct {
	annotationStore *store.Store
	typeReader      schema.AnnotationTypeReader
	hub             *Hub
	// spawn runs a broadcast task on a Server-tracked goroutine so shutdown
	// drains it before the store closes. The server wires it in Listen.
	spawn func(fn func(context.Context))
}

// async runs fn through the server-tracked spawner. A handler constructed
// without one (not a path the server produces) degrades to running inline,
// which is always safe: the store is still open for the request's duration.
func (h *annotationHandler) async(fn func(context.Context)) {
	if h.spawn != nil {
		h.spawn(fn)
		return
	}
	fn(context.Background())
}

// createAnnotationRequest is the JSON body for POST /api/v1/annotations.
// Canonical definition is in the github.com/peasant-labs/schema module; this
// alias prevents drift.
type createAnnotationRequest = schema.CreateAnnotationRequest

// createAnnotationResponse is the JSON body returned on 201.
// Canonical definition is in the github.com/peasant-labs/schema module; this
// alias prevents drift.
type createAnnotationResponse = schema.CreateAnnotationResponse

// batchCreateAnnotationsRequest is the JSON body for POST /api/v1/annotations/batch.
type batchCreateAnnotationsRequest = schema.BatchCreateAnnotationsRequest

// batchCreateAnnotationsResponse is the JSON body returned on 201 for batch endpoint.
type batchCreateAnnotationsResponse = schema.BatchCreateAnnotationsResponse

// batchCreateAnnotationsErrorResponse is the JSON body returned on 400 for batch endpoint.
type batchCreateAnnotationsErrorResponse = schema.BatchCreateAnnotationsErrorResponse

// handleGetAnnotations handles GET /api/v1/annotations?session_id=X.
// Returns all non-superseded annotations for the given session as
// []schema.AnnotationSummary, or 400 if session_id is missing, 503 if the
// store is unavailable, and 500 on other errors.
func (h *annotationHandler) handleGetAnnotations(w http.ResponseWriter, r *http.Request) {
	if h.annotationStore == nil {
		http.Error(w, `{"error":"annotation store not available"}`, http.StatusServiceUnavailable)
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		http.Error(w, `{"error":"missing required query param: session_id"}`, http.StatusBadRequest)
		return
	}

	rows, err := h.annotationStore.GetAnnotationsForSession(r.Context(), sessionID)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch annotations"}`, http.StatusInternalServerError)
		return
	}

	// Session-level lookup omits entry-level (per-turn) annotations — including
	// ingest-generated rule labels — so fetch and append those too.
	entryRows, err := h.annotationStore.GetEntryAnnotationsForSession(r.Context(), sessionID)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch annotations"}`, http.StatusInternalServerError)
		return
	}
	rows = append(rows, entryRows...)
	associationRows, err := h.annotationStore.GetAssociationAnnotationsForSession(r.Context(), sessionID)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch annotations"}`, http.StatusInternalServerError)
		return
	}
	rows = append(rows, associationRows...)

	summaries := annotationRowsToSummaries(rows)

	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	if err := json.NewEncoder(w).Encode(summaries); err != nil {
		http.Error(w, `{"error":"failed to marshal annotations"}`, http.StatusInternalServerError)
	}
}

// handleGetAnnotationTypes handles GET /api/v1/annotation-types.
// Accepts optional query params: status, origin.
// Returns []schema.AnnotationTypeSummary with family and class resolved via JOIN.
func (h *annotationHandler) handleGetAnnotationTypes(w http.ResponseWriter, r *http.Request) {
	if h.annotationStore == nil {
		http.Error(w, `{"error":"annotation store not available"}`, http.StatusServiceUnavailable)
		return
	}

	statusFilter := r.URL.Query().Get("status")
	originFilter := r.URL.Query().Get("origin")

	rows, err := h.annotationStore.ListAnnotationTypesWithFamily(r.Context(), statusFilter, originFilter)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch annotation types"}`, http.StatusInternalServerError)
		return
	}

	summaries := make([]schema.AnnotationTypeSummary, 0, len(rows))
	for _, row := range rows {
		var permissibleValues []string
		if row.ValueConstraint != "" && row.ValueDomainKind == schema.DomainEnumerated {
			if err := json.Unmarshal([]byte(row.ValueConstraint), &permissibleValues); err != nil {
				http.Error(w, `{"error":"failed to parse annotation type value constraint"}`, http.StatusInternalServerError)
				return
			}
		}
		vd := schema.ValueDomain{
			Kind:              row.ValueDomainKind,
			Datatype:          row.Datatype,
			PermissibleValues: permissibleValues,
		}
		// For described domains keep the constraint spec; for enumerated it is not needed.
		if row.ValueDomainKind == schema.DomainDescribed {
			vd.ConstraintSpec = row.ValueConstraint
		}

		var prioOverride *int
		if row.PriorityOverride != nil {
			v := int(*row.PriorityOverride)
			prioOverride = &v
		}

		summaries = append(summaries, schema.AnnotationTypeSummary{
			TypeID:             row.TypeID,
			Version:            row.Version,
			DisplayName:        row.DisplayName,
			Description:        row.Description,
			Family:             row.Family,
			Class:              row.Class,
			ValueDomain:        vd,
			LowerIsBetter:      row.LowerIsBetter,
			Status:             row.Status,
			Origin:             row.Origin,
			PriorityOverride:   prioOverride,
			AllowedTargetKinds: row.AllowedTargetKinds,
		})
	}

	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	if err := json.NewEncoder(w).Encode(summaries); err != nil {
		http.Error(w, `{"error":"failed to marshal annotation types"}`, http.StatusInternalServerError)
	}
}

// handleCreateAnnotation handles POST /api/v1/annotations.
// Decodes the request body, validates the annotation value, resolves the
// annotator (by name, defaulting to "api"), and calls CreateAnnotation.
// Returns 201 with {"id": <new annotation DB id>} on success.
func (h *annotationHandler) handleCreateAnnotation(w http.ResponseWriter, r *http.Request) {
	if h.annotationStore == nil {
		http.Error(w, `{"error":"annotation store not available"}`, http.StatusServiceUnavailable)
		return
	}

	var req createAnnotationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.SessionID == "" {
		http.Error(w, `{"error":"missing required field: sessionId"}`, http.StatusBadRequest)
		return
	}
	if req.TypeID == "" {
		http.Error(w, `{"error":"missing required field: typeId"}`, http.StatusBadRequest)
		return
	}
	if req.Value == "" {
		http.Error(w, `{"error":"missing required field: value"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Validate value against the annotation type.
	if err := h.typeReader.ValidateValue(ctx, req.TypeID, req.Value); err != nil {
		http.Error(w, `{"error":"invalid annotation value: `+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	// Resolve annotation type UUID.
	annotationTypeID, err := h.annotationStore.GetAnnotationTypeID(ctx, req.TypeID)
	if err != nil || annotationTypeID == "" {
		http.Error(w, `{"error":"annotation type not found"}`, http.StatusBadRequest)
		return
	}

	// Resolve annotator by name. Default to "api" annotator if not specified.
	annotatorName := req.AnnotatorName
	if annotatorName == "" {
		annotatorName = "api"
	}
	annotatorID, err := h.annotationStore.GetAnnotatorIDByName(ctx, annotatorName)
	if err != nil {
		http.Error(w, `{"error":"failed to resolve annotator"}`, http.StatusInternalServerError)
		return
	}
	if annotatorID == "" {
		http.Error(w, `{"error":"annotator not found: `+annotatorName+`"}`, http.StatusBadRequest)
		return
	}

	sid := req.SessionID
	params := store.CreateAnnotationParams{
		SessionID:        &sid,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: annotationTypeID,
		Value:            req.Value,
		IsPrimary:        req.IsPrimary,
		Confidence:       req.Confidence,
		Reason:           req.Reason,
	}

	newID, err := h.annotationStore.CreateAnnotation(ctx, params)
	if err != nil {
		http.Error(w, `{"error":"failed to create annotation"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(createAnnotationResponse{ID: newID}); err != nil {
		// Response header already sent; log but cannot change status.
		_ = err
	}

	// Broadcast annotation update to WebSocket subscribers on a tracked
	// goroutine: shutdown drains it before the store can be closed.
	if h.hub != nil {
		sessionID := req.SessionID
		h.async(func(ctx context.Context) {
			rows, err := h.annotationStore.GetAnnotationsForSession(ctx, sessionID)
			if err != nil {
				return
			}
			h.hub.Broadcast(TopicAnnotations, ServerMessage{
				Type: MsgAnnotations,
				Data: &AnnotationsPayload{
					Axis:        AxisSession,
					ID:          sessionID,
					Annotations: annotationRowsToSummaries(rows),
				},
			})
		})
	}
}

// handleBatchCreateAnnotations handles POST /api/v1/annotations/batch.
// All annotations are committed as an all-or-nothing SQLite transaction via
// store.BatchCreateAnnotations. On validation failure, returns 400 with the
// zero-based index of the first failing annotation and no DB writes are committed.
// On success, returns 201 with all created annotation UUIDs in request order.
func (h *annotationHandler) handleBatchCreateAnnotations(w http.ResponseWriter, r *http.Request) {
	if h.annotationStore == nil {
		http.Error(w, `{"error":"annotation store not available"}`, http.StatusServiceUnavailable)
		return
	}

	var req batchCreateAnnotationsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if len(req.Annotations) == 0 {
		http.Error(w, `{"error":"annotations array must not be empty"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Validate + resolve all annotations before any DB writes.
	params := make([]store.CreateAnnotationParams, len(req.Annotations))
	for i, ann := range req.Annotations {
		if ann.TypeID == "" {
			errResp, _ := json.Marshal(batchCreateAnnotationsErrorResponse{
				Error:        fmt.Sprintf("annotation[%d]: missing required field: typeId", i),
				FailingIndex: i,
			})
			w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
			w.WriteHeader(http.StatusBadRequest)
			w.Write(errResp) //nolint:errcheck
			return
		}
		if ann.Value == "" {
			errResp, _ := json.Marshal(batchCreateAnnotationsErrorResponse{
				Error:        fmt.Sprintf("annotation[%d]: missing required field: value", i),
				FailingIndex: i,
			})
			w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
			w.WriteHeader(http.StatusBadRequest)
			w.Write(errResp) //nolint:errcheck
			return
		}
		// Session ID required unless it's a pure meta-annotation.
		if ann.SessionID == "" && ann.TargetAnnotationID == nil {
			errResp, _ := json.Marshal(batchCreateAnnotationsErrorResponse{
				Error:        fmt.Sprintf("annotation[%d]: sessionId required when targetAnnotationId is absent", i),
				FailingIndex: i,
			})
			w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
			w.WriteHeader(http.StatusBadRequest)
			w.Write(errResp) //nolint:errcheck
			return
		}

		// Validate value against the annotation type.
		if err := h.typeReader.ValidateValue(ctx, ann.TypeID, ann.Value); err != nil {
			errResp, _ := json.Marshal(batchCreateAnnotationsErrorResponse{
				Error:        fmt.Sprintf("annotation[%d]: invalid value for %s: %s", i, ann.TypeID, err.Error()),
				FailingIndex: i,
			})
			w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
			w.WriteHeader(http.StatusBadRequest)
			w.Write(errResp) //nolint:errcheck
			return
		}

		// Resolve annotation type UUID.
		annotationTypeID, err := h.annotationStore.GetAnnotationTypeID(ctx, ann.TypeID)
		if err != nil || annotationTypeID == "" {
			errResp, _ := json.Marshal(batchCreateAnnotationsErrorResponse{
				Error:        fmt.Sprintf("annotation[%d]: annotation type not found: %s", i, ann.TypeID),
				FailingIndex: i,
			})
			w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
			w.WriteHeader(http.StatusBadRequest)
			w.Write(errResp) //nolint:errcheck
			return
		}

		// Resolve annotator by name; default to "api".
		annotatorName := ann.AnnotatorName
		if annotatorName == "" {
			annotatorName = "api"
		}
		annotatorID, err := h.annotationStore.GetAnnotatorIDByName(ctx, annotatorName)
		if err != nil {
			http.Error(w, `{"error":"failed to resolve annotator"}`, http.StatusInternalServerError)
			return
		}
		if annotatorID == "" {
			errResp, _ := json.Marshal(batchCreateAnnotationsErrorResponse{
				Error:        fmt.Sprintf("annotation[%d]: annotator not found: %s", i, annotatorName),
				FailingIndex: i,
			})
			w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
			w.WriteHeader(http.StatusBadRequest)
			w.Write(errResp) //nolint:errcheck
			return
		}

		// Build store params — target arm selection.
		p := store.CreateAnnotationParams{
			AnnotatorID:      annotatorID,
			AnnotationTypeID: annotationTypeID,
			Value:            ann.Value,
			IsPrimary:        ann.IsPrimary,
			Confidence:       ann.Confidence,
			Reason:           ann.Reason,
		}
		switch {
		case ann.TargetAnnotationID != nil:
			p.AnnotationID = ann.TargetAnnotationID
		case ann.TargetEntryIndex != nil:
			endIdx := 0
			if ann.TargetEntryEndIndex != nil {
				endIdx = *ann.TargetEntryEndIndex
			}
			p.EntryTarget = &store.EntryTarget{
				SessionID:  ann.SessionID,
				EntryIndex: *ann.TargetEntryIndex,
				EndIndex:   endIdx,
			}
		default:
			sid := ann.SessionID
			p.SessionID = &sid
		}
		params[i] = p
	}

	// All validation passed — commit the batch atomically.
	ids, err := h.annotationStore.BatchCreateAnnotations(ctx, params)
	if err != nil {
		http.Error(w, `{"error":"failed to create annotations"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(batchCreateAnnotationsResponse{IDs: ids}); err != nil {
		_ = err
	}

	// Broadcast updates for any session-level targets.
	if h.hub != nil {
		// Collect unique session IDs affected by this batch.
		seen := make(map[string]bool)
		for _, ann := range req.Annotations {
			if ann.SessionID != "" && !seen[ann.SessionID] {
				seen[ann.SessionID] = true
				sessionID := ann.SessionID
				h.async(func(ctx context.Context) {
					rows, err := h.annotationStore.GetAnnotationsForSession(ctx, sessionID)
					if err != nil {
						return
					}
					h.hub.Broadcast(TopicAnnotations, ServerMessage{
						Type: MsgAnnotations,
						Data: &AnnotationsPayload{
							Axis:        AxisSession,
							ID:          sessionID,
							Annotations: annotationRowsToSummaries(rows),
						},
					})
				})
			}
		}
		// Annotations affect quality metrics (e.g. effectiveAnnotations) —
		// trigger an async quality refresh for subscribers, tracked like the
		// broadcasts above so shutdown drains it too.
		h.async(h.hub.RefreshQuality)
	}
}
