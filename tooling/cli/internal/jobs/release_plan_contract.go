package jobs

import distribution "go.putnami.dev/protocol/distribution"

// releasePlanProtocolVersion identifies this closed process-private transport.
const releasePlanProtocolVersion = 1

// releasePlanMember is one selected publication before its artifact bytes exist.
type releasePlanMember struct {
	Ecosystem            distribution.Ecosystem `json:"ecosystem"`
	Coordinate           string                 `json:"coordinate"`
	Version              string                 `json:"version"`
	SourceRevision       string                 `json:"sourceRevision"`
	SelectionFingerprint string                 `json:"selectionFingerprint"`
}

// releasePlanHandoff freezes the exact tuple the runner must validate through Delivery.
// PlanDigest hashes the JSON with that final field omitted.
type releasePlanHandoff struct {
	ProtocolVersion  int                 `json:"protocolVersion"`
	Namespace        string              `json:"namespace"`
	SourceRevision   string              `json:"sourceRevision"`
	Channels         []string            `json:"channels"`
	ImmutableChannel string              `json:"immutableChannel,omitempty"`
	Members          []releasePlanMember `json:"members"`
	PlanDigest       string              `json:"planDigest,omitempty"`
}

// releasePlanAcknowledgment confirms only the frozen tuple, never a registry credential.
type releasePlanAcknowledgment struct {
	ProtocolVersion int    `json:"protocolVersion"`
	PlanDigest      string `json:"planDigest"`
}
