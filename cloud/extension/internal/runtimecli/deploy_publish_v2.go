package runtimecli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"go.putnami.dev/client"
	controlapiclient "go.putnami.dev/cloud/clients/control-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	perrors "go.putnami.dev/errors"
)

const (
	publishV2InputVersion  = 1
	publishV2MaxBytes      = 4 * 1024 * 1024
	publishV2MaxProjects   = 4096
	publishV2MaxMigrations = 64
)

var (
	publishV2SourceRevision = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	publishV2Project        = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,199}$`)
	publishV2Digest         = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	publishV2ReleaseSetID   = regexp.MustCompile(`^rs_[0-9a-f]{64}$`)
)

// publishV2DeployInput is the credential-free handoff from a release-set
// coordinator to the Cloud deploy command. It carries only immutable owner
// references and exact member selectors; Control revalidates all of them.
type publishV2DeployInput struct {
	ProtocolVersion                       int                     `json:"protocolVersion"`
	SourceRevision                        string                  `json:"sourceRevision"`
	ExpectedEnvironmentDefinitionRevision int64                   `json:"expectedEnvironmentDefinitionRevision"`
	ReleaseSetRef                         publishV2ReleaseSetRef  `json:"releaseSetRef"`
	Projects                              []publishV2ProjectInput `json:"projects"`
}

type publishV2ReleaseSetRef struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

type publishV2MemberSelector struct {
	Ecosystem      string `json:"ecosystem"`
	Coordinate     string `json:"coordinate"`
	Version        string `json:"version"`
	ArtifactDigest string `json:"artifact_digest"`
}

type publishV2ProjectInput struct {
	Name   string                  `json:"name"`
	Image  publishV2MemberSelector `json:"image"`
	Config publishV2MemberSelector `json:"config"`
	// Migrations are the project's published migration bundle members, in the
	// exact order they must be applied. Absent for a workload that owns no
	// schema. Control's preflight refuses a member outside the accepted closure
	// and a project that selects one member as both Config and migration, so
	// the file only has to be well-formed here; the closure is Control's call.
	Migrations []publishV2MemberSelector `json:"migrations,omitempty"`
}

type publishV2AcceptanceReceipt struct {
	WorkspaceID    string    `json:"workspace_id"`
	Revision       int64     `json:"revision"`
	Digest         string    `json:"digest"`
	SourceRevision string    `json:"source_revision"`
	AcceptedAt     time.Time `json:"accepted_at"`
}

type publishV2ControlRequest struct {
	EnvironmentDefinitionRevision int64                   `json:"environment_definition_revision"`
	ReleaseSetRef                 publishV2ReleaseSetRef  `json:"release_set_ref"`
	Projects                      []publishV2ProjectInput `json:"projects"`
}

// DeployPublishV2 accepts the immutable environment definition at Source and
// submits the exact accepted release-set selection through Control's ordinary
// Publish v2 path. It never resolves a channel head or reads legacy manifests.
func DeployPublishV2(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	if clicore.FirstPositional(args) != "" {
		return clicore.NewError("usage: putnami cloud deploy publish-v2 --request-file <path>", clicore.ExitUsage)
	}
	path := clicore.StringParam(params, "request-file", "requestFile")
	if path == "" {
		return clicore.NewError("usage: putnami cloud deploy publish-v2 --request-file <path>", clicore.ExitUsage)
	}
	input, err := readPublishV2DeployInput(path)
	if err != nil {
		return err
	}
	ctx, err := newDeployCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	return deployPublishV2(params, ctx, input, ioctx)
}

func readPublishV2DeployInput(path string) (publishV2DeployInput, error) {
	var input publishV2DeployInput
	file, err := os.Open(path) //nolint:gosec // The operator explicitly selects this non-secret request document.
	if err != nil {
		return input, clicore.NewError("read publish-v2 request file: "+err.Error(), clicore.ExitUsage)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return input, clicore.NewError("publish-v2 request file must be a regular file", clicore.ExitUsage)
	}
	if info.Size() <= 0 || info.Size() > publishV2MaxBytes {
		return input, clicore.NewError("publish-v2 request file is empty or exceeds its size limit", clicore.ExitUsage)
	}
	data, err := io.ReadAll(io.LimitReader(file, publishV2MaxBytes+1))
	if err != nil || len(data) > publishV2MaxBytes {
		return input, clicore.NewError("read publish-v2 request file", clicore.ExitUsage)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, clicore.NewError("publish-v2 request file is invalid", clicore.ExitUsage)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return input, clicore.NewError("publish-v2 request file contains trailing data", clicore.ExitUsage)
	}
	if err := validatePublishV2DeployInput(input); err != nil {
		return input, err
	}
	return input, nil
}

func validatePublishV2DeployInput(input publishV2DeployInput) error {
	if input.ProtocolVersion != publishV2InputVersion || !publishV2SourceRevision.MatchString(input.SourceRevision) ||
		input.ExpectedEnvironmentDefinitionRevision < 0 || input.ExpectedEnvironmentDefinitionRevision == int64(^uint64(0)>>1) ||
		!publishV2ReleaseSetID.MatchString(input.ReleaseSetRef.ID) ||
		input.ReleaseSetRef.Digest != "sha256:"+strings.TrimPrefix(input.ReleaseSetRef.ID, "rs_") ||
		len(input.Projects) == 0 || len(input.Projects) > publishV2MaxProjects {
		return clicore.NewError("publish-v2 request requires a supported protocol, immutable source revision, release set, and projects", clicore.ExitUsage)
	}
	seen := make(map[string]struct{}, len(input.Projects))
	for _, project := range input.Projects {
		if !publishV2Project.MatchString(project.Name) || !validPublishV2Member(project.Image, "oci") || !validPublishV2Member(project.Config, "put") {
			return clicore.NewError("publish-v2 projects require exact OCI image and Put config selectors", clicore.ExitUsage)
		}
		if len(project.Migrations) > publishV2MaxMigrations {
			return clicore.NewError("publish-v2 projects carry at most 64 ordered migration selectors", clicore.ExitUsage)
		}
		for _, migration := range project.Migrations {
			if !validPublishV2Member(migration, "put") {
				return clicore.NewError("publish-v2 migration selectors must be exact Put members", clicore.ExitUsage)
			}
		}
		if _, duplicate := seen[project.Name]; duplicate {
			return clicore.NewError("publish-v2 project names must be unique", clicore.ExitUsage)
		}
		seen[project.Name] = struct{}{}
	}
	return nil
}

func validPublishV2Member(member publishV2MemberSelector, ecosystem string) bool {
	return member.Ecosystem == ecosystem && member.Coordinate != "" && len(member.Coordinate) <= 512 &&
		member.Version != "" && len(member.Version) <= 256 &&
		strings.TrimSpace(member.Coordinate) == member.Coordinate && strings.TrimSpace(member.Version) == member.Version &&
		!strings.ContainsAny(member.Coordinate+member.Version, "\x00\r\n") && publishV2Digest.MatchString(member.ArtifactDigest)
}

func deployPublishV2(params map[string]any, ctx *deployCtx, input publishV2DeployInput, ioctx clicore.IO) error {
	if ioctx.Now == nil {
		ioctx.Now = time.Now
	}
	reqCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	receipt, err := acceptPublishV2Environment(reqCtx, ctx, input)
	if err != nil {
		return err
	}
	releaseID, err := generateReleaseID()
	if err != nil {
		return err
	}
	body := map[string]any{
		"environment": ctx.environment,
		"release_id":  releaseID,
		"publish_v2": publishV2ControlRequest{
			EnvironmentDefinitionRevision: receipt.Revision,
			ReleaseSetRef:                 input.ReleaseSetRef,
			Projects:                      append([]publishV2ProjectInput(nil), input.Projects...),
		},
	}
	targets := make([]deployTarget, len(input.Projects))
	for index, project := range input.Projects {
		targets[index] = deployTarget{app: project.Name, version: project.Image.Version, digest: project.Image.ArtifactDigest, imageName: project.Image.Coordinate}
	}
	ctx.localTaskGraphProjectCount = len(targets)
	ctx.releaseWallStarted = ioctx.Now()
	deadline := ioctx.Now().Add(parseDeployTimeout(params))
	submitStarted := ioctx.Now()
	response, status, err := submitRelease(reqCtx, ctx, body, deadline, ioctx)
	if err != nil {
		return err
	}
	submitMS := ioctx.Now().Sub(submitStarted).Milliseconds()
	if submitMS < 0 {
		submitMS = 0
	}
	ctx.releaseSubmitMS = &submitMS

	if status == http.StatusAccepted && clicore.Truthy(clicore.Param(params, "wait")) {
		deadline = ioctx.Now().Add(parseDeployTimeout(params))
		progress := emitDeployProgress(params, ioctx, deployProgress{}, response)
		for response.State == "Provisioning" && ioctx.Now().Before(deadline) {
			if err := sleepCtx(reqCtx, DeployPollInterval); err != nil {
				return clicore.NewError("deploy wait canceled by user", clicore.ExitUsage)
			}
			var next *deployResponse
			var nextStatus int
			if response.Async {
				next, nextStatus, err = deployStatusRequest(reqCtx, ctx, releaseID)
			} else {
				next, nextStatus, err = deployRequest(reqCtx, ctx, body)
			}
			if err != nil {
				if isTransientDeployError(err) && ioctx.Now().Before(deadline) {
					continue
				}
				return err
			}
			response, status = next, nextStatus
			progress = emitDeployProgress(params, ioctx, progress, response)
			if status != http.StatusAccepted && status != http.StatusOK {
				break
			}
		}
		if response.State == "Provisioning" {
			_ = renderRelease(params, ctx, ioctx, releaseID, targets, response)
			return clicore.NewError(fmt.Sprintf("deploy %s did not reach Ready within --timeout", releaseID), clicore.ExitAPI)
		}
	}
	return renderRelease(params, ctx, ioctx, releaseID, targets, response)
}

// acceptPublishV2Environment accepts the Source environment definition
// through control-api's generated CreateV1WorkspacesEnvironmentDefinitionsAccept,
// with clicore.CallWithSession's single 401 re-mint. A refusal names its
// status line: the generated client withholds the provider's free-text
// message.
func acceptPublishV2Environment(reqCtx context.Context, ctx *deployCtx, input publishV2DeployInput) (publishV2AcceptanceReceipt, error) {
	var receipt publishV2AcceptanceReceipt
	control, err := controlClient(ctx.WorkspaceContext)
	if err != nil {
		return receipt, err
	}
	expected, source := input.ExpectedEnvironmentDefinitionRevision, input.SourceRevision
	accepted, err := clicore.CallWithSession(reqCtx, ctx.WorkspaceContext, func(callCtx context.Context) (*controlapiclient.EnvironmentDefinitionAcceptance, error) {
		return control.CreateV1WorkspacesEnvironmentDefinitionsAccept(callCtx, controlapiclient.CreateV1WorkspacesEnvironmentDefinitionsAcceptInput{
			Path: controlapiclient.CreateV1WorkspacesEnvironmentDefinitionsAcceptPath{Workspace: ctx.WorkspaceID},
			Body: controlapiclient.SourceAcceptInput{ExpectedRevision: &expected, SourceRevision: &source},
		})
	})
	if err != nil {
		if status := clicore.ServiceStatus(err); status != 0 {
			code := clicore.ExitAPI
			if status == http.StatusUnauthorized {
				code = clicore.ExitAuth
			}
			return receipt, clicore.NewError("environment definition acceptance: "+clicore.StatusLine(status), code)
		}
		if reqCtx.Err() != nil {
			return receipt, clicore.NewError("environment definition acceptance canceled", clicore.ExitUsage)
		}
		if perrors.Is(err, client.CodeClientResponse) {
			return receipt, clicore.NewError("environment definition acceptance returned invalid JSON", clicore.ExitAPI)
		}
		return receipt, clicore.NewError("environment definition acceptance request failed", clicore.ExitAPI)
	}
	receipt = publishV2AcceptanceReceipt{
		WorkspaceID:    clicore.Deref(accepted.WorkspaceId),
		Revision:       clicore.Deref(accepted.Revision),
		Digest:         clicore.Deref(accepted.Digest),
		SourceRevision: clicore.Deref(accepted.SourceRevision),
		AcceptedAt:     clicore.Deref(accepted.AcceptedAt),
	}
	// A new acceptance is expected+1. An exact Source-coordinate replay may
	// return its earlier immutable revision, but can never return a later one.
	if receipt.WorkspaceID != ctx.WorkspaceID || receipt.SourceRevision != input.SourceRevision || receipt.Revision <= 0 ||
		receipt.Revision > input.ExpectedEnvironmentDefinitionRevision+1 || !publishV2Digest.MatchString(receipt.Digest) || receipt.AcceptedAt.IsZero() {
		return receipt, clicore.NewError("environment definition acceptance returned a different identity", clicore.ExitAPI)
	}
	return receipt, nil
}
