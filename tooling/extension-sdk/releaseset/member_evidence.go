package releaseset

import (
	"context"
	"encoding/json"
	"fmt"

	runtimeproto "go.putnami.dev/protocol/runtime"
)

// MemberEvidenceFileEnv is private framework-to-provider provenance transport.
const MemberEvidenceFileEnv = runtimeproto.ReleaseSetMembersFileEnv

// MemberEvidence names a selected publication and its frozen producer route.
// Provider consumers still verify immutable owner facts; these are assertions.
type MemberEvidence struct {
	Project    string `json:"project"`
	Ecosystem  string `json:"ecosystem"`
	Coordinate string `json:"coordinate"`
	Version    string `json:"version"`
	Digest     string `json:"digest"`
	Publisher  string `json:"publisher"`
	Command    string `json:"command"`
	Step       string `json:"step"`
}
type memberEvidenceKey struct{}

// WithMemberEvidence attaches an owned copy to one reserved provider call.
func WithMemberEvidence(ctx context.Context, members []MemberEvidence) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, memberEvidenceKey{}, append([]MemberEvidence(nil), members...))
}

func memberEvidenceBytes(ctx context.Context) ([]byte, bool, error) {
	members, present := MemberEvidenceFromContext(ctx)
	if !present {
		return nil, false, nil
	}
	data, err := json.Marshal(struct {
		Version int              `json:"version"`
		Members []MemberEvidence `json:"members"`
	}{1, members})
	if err != nil {
		return nil, true, err
	}
	if len(data) > 4<<20 {
		return nil, true, fmt.Errorf("member evidence exceeds private transport limit")
	}
	return data, true, nil
}

// MemberEvidenceFromContext returns an owned copy for reserved provider adapters.
func MemberEvidenceFromContext(ctx context.Context) ([]MemberEvidence, bool) {
	if ctx == nil {
		return nil, false
	}
	members, present := ctx.Value(memberEvidenceKey{}).([]MemberEvidence)
	return append([]MemberEvidence(nil), members...), present
}
