package localslice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	transportstore "github.com/GentleKingson/ocservia/control-plane/internal/localslice/store"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func (s *Service) LastEventID(ctx context.Context) ([]byte, error) {
	var id uuid.UUID
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := transportstore.Ingress(tx)
		if err != nil {
			return err
		}
		id, err = store.LastEventID(ctx)
		return err
	})
	if errors.Is(err, database.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read event cursor: %w", err)
	}
	return id[:], nil
}
func (s *Service) Ingest(ctx context.Context, event *transportv1.TransportEvent) error {
	ctx = propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": event.GetTraceparent()})
	ctx, span := otel.Tracer("ocservia.localslice").Start(ctx, "local_slice.event.ingest", trace.WithSpanKind(trace.SpanKindConsumer))
	defer span.End()
	observedAt := s.now()
	eventID, err := uuid.FromBytes(event.GetEventId())
	if err != nil || eventID.Version() != 7 {
		return errors.New("transport event_id must be UUIDv7")
	}
	nodeID, err := uuid.FromBytes(event.GetNodeId())
	if err != nil || nodeID.Version() != 7 {
		return errors.New("transport node_id must be UUIDv7")
	}
	return database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := transportstore.Ingress(tx)
		if err != nil {
			return err
		}
		if err := store.SaveBusiness(ctx); err != nil {
			return fmt.Errorf("create transport event savepoint: %w", err)
		}
		workspaceID, inserted, err := s.ingestTransportEventTx(ctx, tx, eventID, nodeID, event, observedAt)
		if err != nil {
			var permanent *permanentInvalidEvent
			if !errors.As(err, &permanent) {
				return err
			}
			if rollbackErr := store.RollbackBusiness(ctx); rollbackErr != nil {
				return fmt.Errorf("rollback permanently invalid transport event: %w", rollbackErr)
			}
			if releaseErr := store.ReleaseBusiness(ctx); releaseErr != nil {
				return fmt.Errorf("release transport event savepoint: %w", releaseErr)
			}
			if err := quarantineTransportEvent(ctx, store, eventID, nodeID, event, workspaceID, observedAt, permanent); err != nil {
				return err
			}
		} else {
			if inserted && event.GetType() == transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_CONNECTED && s.commandRecovery != nil && s.recoveryAuthority != nil {
				connectionID, fenced, termErr := connectedOwnerTerm(event)
				if termErr != nil {
					return termErr
				}
				if fenced && s.recoveryAuthority.OwnsTerm([16]byte(nodeID), connectionID, int64(event.GetOwnerEpoch())) {
					recovered, recoveryErr := s.commandRecovery.RecoverAmbiguousDispatched(ctx, tx, operationstore.OwnerReconnect{
						NodeID: nodeID, ConnectionID: connectionID, OwnerEpoch: event.GetOwnerEpoch(),
						ObservedAt: observedAt, Limit: operationstore.DefaultReconnectRecoveryLimit,
					})
					if recoveryErr != nil && !errors.Is(recoveryErr, connectionowner.ErrNotOwner) {
						return fmt.Errorf("recover commands after authoritative reconnect: %w", recoveryErr)
					}
					span.SetAttributes(attribute.Int("command.recovery.scheduled", recovered))
				}
			}
			if err := store.ReleaseBusiness(ctx); err != nil {
				return fmt.Errorf("release transport event savepoint: %w", err)
			}
			if err := advanceTransportCursor(ctx, store, eventID, observedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) ingestTransportEventTx(ctx context.Context, tx database.Tx, eventID, nodeID uuid.UUID, event *transportv1.TransportEvent, observedAt time.Time) (uuid.UUID, bool, error) {
	store, err := transportstore.Ingress(tx)
	if err != nil {
		return uuid.Nil, false, err
	}
	trust, err := store.LockTrust(ctx, nodeID, event.GetEndpointId())
	if errors.Is(err, database.ErrNotFound) {
		return uuid.Nil, false, invalidEvent("node_endpoint_not_active", "transport event node and EndpointID are not authoritatively active")
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("check authoritative node ingress trust: %w", err)
	}
	workspaceID, nodeStatus, endpointState := trust.WorkspaceID, trust.NodeStatus, trust.EndpointState
	eventType, err := eventName(event.GetType())
	if err != nil {
		return workspaceID, false, invalidEvent("unsupported_event_type", err.Error())
	}
	if eventType == "connected" {
		if _, _, err := connectedOwnerTerm(event); err != nil {
			return workspaceID, false, invalidEvent("invalid_owner_term", err.Error())
		}
	}
	activeIngress := (nodeStatus == "active" || nodeStatus == "offline") && endpointState == "active"
	// Revocation commits the tombstone before transportd closes the old session.
	// Only its exact node/endpoint terminal event may cross that closed trust boundary.
	revokedTerminalDisconnect := eventType == "disconnected" && nodeStatus == "revoked" && endpointState == "revoked"
	if !activeIngress && !revokedTerminalDisconnect {
		return workspaceID, false, invalidEvent("node_endpoint_not_active", "transport event node is not authoritatively active")
	}
	if len(event.GetEndpointId()) != 32 {
		return workspaceID, false, invalidEvent("invalid_endpoint_id", "transport event endpoint_id must be 32 bytes")
	}
	if !validTraceparent(event.GetTraceparent()) {
		return workspaceID, false, invalidEvent("invalid_traceparent", "transport event traceparent is invalid")
	}
	if len(event.GetPayload()) > 1<<20 {
		return workspaceID, false, invalidEvent("payload_too_large", "transport event payload exceeds 1 MiB")
	}
	occurredAt := event.GetOccurredAt()
	if occurredAt == nil || occurredAt.CheckValid() != nil {
		return workspaceID, false, invalidEvent("invalid_occurred_at", "transport event occurred_at is invalid")
	}
	occurredTime := occurredAt.AsTime()
	if occurredTime.Before(observedAt.Add(-maxTransportEventAge)) || occurredTime.After(observedAt.Add(telemetrystore.MaxTelemetrySkew)) {
		return workspaceID, false, invalidEvent("occurred_at_out_of_range", "transport event occurred_at is outside the accepted retention window")
	}
	at, err := value.FromTime(occurredTime)
	if err != nil {
		return workspaceID, false, err
	}
	inserted, err := store.InsertEvent(ctx, transportstore.TransportEvent{ID: eventID, NodeID: nodeID, Type: eventType, At: at, Traceparent: event.GetTraceparent(), Payload: event.GetPayload()})
	if err != nil {
		return workspaceID, false, fmt.Errorf("insert transport event: %w", err)
	}
	if !inserted {
		return workspaceID, false, nil
	}
	if eventType == "telemetry" {
		if _, err := telemetrystore.NewBackend(s.backend).IngestWireTransaction(ctx, tx, nodeID, event.GetPayload()); err != nil {
			if errors.Is(err, telemetrystore.ErrInvalidTelemetry) {
				return workspaceID, false, invalidEvent("invalid_telemetry", err.Error())
			}
			return workspaceID, false, fmt.Errorf("ingest telemetry payload: %w", err)
		}
	}
	structuredCommandResult := eventType == "command_result"
	if structuredCommandResult {
		if err := IngestCommandResult(ctx, tx, eventID, nodeID, event.GetPayload(), occurredTime, observedAt, s.signer); err != nil {
			return workspaceID, false, err
		}
		if err := s.waitAtResultCommitBarrier(ctx, event, observedAt); err != nil {
			return workspaceID, false, err
		}
	}
	status := "active"
	if eventType == "disconnected" {
		status = "offline"
	}
	if eventType != "telemetry" {
		// Routine same-state signals must not invalidate node mutation preconditions.
		if err := store.NodeStatus(ctx, nodeID, status, at); err != nil {
			return workspaceID, false, fmt.Errorf("update node from transport event: %w", err)
		}
	}
	operationState := "running"
	switch eventType {
	case "simulation_result":
		operationState = "succeeded"
	case "error":
		operationState = "failed"
	case "disconnected":
		operationState = "unknown"
	}
	if eventType == "disconnected" || (eventType != "telemetry" && eventType != "path_changed" && !structuredCommandResult) {
		if err := store.SimulationOutcome(ctx, nodeID, operationState, event.GetTraceparent(), eventType == "disconnected", at); err != nil {
			return workspaceID, false, fmt.Errorf("update operation from transport event: %w", err)
		}
	}
	return workspaceID, true, nil
}

func quarantineTransportEvent(ctx context.Context, store transportstore.IngressStore, eventID, nodeID uuid.UUID, event *transportv1.TransportEvent, workspaceID uuid.UUID, observedAt time.Time, failure *permanentInvalidEvent) error {
	payloadHash := sha256.Sum256(event.GetPayload())
	at, err := value.FromTime(observedAt)
	if err != nil {
		return err
	}
	v := transportstore.Quarantine{EventID: eventID, NodeID: nodeID, Type: int32(event.GetType()), PayloadHash: payloadHash[:], ReasonCode: failure.reasonCode, ReasonDetail: failure.detail, At: at}
	inserted, err := store.InsertQuarantine(ctx, v)
	if err != nil {
		return fmt.Errorf("quarantine permanently invalid transport event: %w", err)
	}
	if !inserted {
		existing, err := store.Quarantine(ctx, eventID)
		if err != nil {
			return fmt.Errorf("read quarantined transport event: %w", err)
		}
		if existing.NodeID != nodeID || existing.Type != v.Type || !bytes.Equal(existing.PayloadHash, payloadHash[:]) {
			return errors.New("transport event ID collides with different quarantine evidence")
		}
	} else if workspaceID != uuid.Nil {
		alertID, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("generate transport quarantine alert ID: %w", err)
		}
		if err := store.QuarantineAlert(ctx, alertID, workspaceID, v); err != nil {
			return fmt.Errorf("emit transport quarantine security alert: %w", err)
		}
	}
	return store.AdvanceCursor(ctx, eventID, at)
}
func advanceTransportCursor(ctx context.Context, store transportstore.IngressStore, eventID uuid.UUID, observedAt time.Time) error {
	at, err := value.FromTime(observedAt)
	if err != nil {
		return err
	}
	if err := store.AdvanceCursor(ctx, eventID, at); err != nil {
		return fmt.Errorf("advance durable transport cursor: %w", err)
	}
	return nil
}
