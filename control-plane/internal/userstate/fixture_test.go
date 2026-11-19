package userstate

import (
	"bytes"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/google/uuid"
)

func mutation(nodeID uuid.UUID, key string, kind MutationKind, name string, version int64) MutationRequest {
	request := MutationRequest{
		NodeID: nodeID, Kind: kind, Name: name, IdempotencyKey: key,
		ExpectedVersion: version, TTL: time.Hour, ActorID: "operator",
		Reason: "ticket", RequestID: "request-" + key, Traceparent: testTraceparent,
	}
	if kind == UserCreate || kind == UserPasswordRotate {
		request.SealedPassword = &SealedSecret{
			Version: 1, Purpose: "user_password", KeyID: "node-key-1",
			Ciphertext: bytes.Repeat([]byte{0xa5}, 64),
		}
	}
	return request
}

func integrationCommandSigner() *commandauth.Signer {
	return testfixture.CommandSigner(2)
}
