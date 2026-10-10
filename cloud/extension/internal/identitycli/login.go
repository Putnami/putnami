package identitycli

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// LoginResult is the outcome of a successful device-authorization login: the
// persisted credential, the resolved user, and the human-facing success line.
// The caller layers any cross-domain follow-up (e.g. registry credential
// provisioning) on top — identity does not reach into other domains.
type LoginResult struct {
	Auth    *clicore.StoredToken
	User    User
	Message string
}

// Login runs the OAuth2 device-authorization flow (printing instructions,
// optionally opening the browser, and polling for the token), persists the
// credential to ~/.putnami/auth.json, and returns the resolved identity. It does
// not provision registry credentials; that is the caller's orchestration.
func Login(params map[string]any, env map[string]string, ioctx clicore.IO) (LoginResult, error) {
	endpoints := clicore.AuthEndpoints(params, env, ioctx.Client, "")
	clientID := clicore.StringParam(params, "client-id", "clientId")
	if clientID == "" {
		clientID = "putnami-cli"
	}
	scope := clicore.StringParam(params, "scope")
	if scope == "" {
		// apikeys:write is required so the resulting JWT can mint the per-
		// registry pkt_* personal access keys that the login flow records for
		// registry-token resolution. intelligence.read enables the hosted
		// Intelligence MCP tools; intelligence.audit is the narrower shared
		// attestation permission and never grants graph writes.
		// intelligence.review.read / intelligence.review.manage let the
		// workspace session drive `putnami intelligence review`; the
		// API still enforces the workspace role behind them. Bundling them
		// into the default keeps `putnami cloud login` a single-prompt flow.
		scope = "openid profile email apikeys:write intelligence.read intelligence.audit intelligence.review.read intelligence.review.manage"
	}
	body := map[string]any{"client_id": clientID, "scope": scope}
	if workspaceID := clicore.ScalarStringParam(params, "workspace", "workspaceId"); workspaceID != "" {
		ioctx.Stderr("warning: --workspace on `putnami cloud login` is deprecated and ignored; run `putnami cloud setup --workspace <id>` instead")
	}

	device, err := requestDeviceAuthorization(ioctx.Client, endpoints.DeviceAuthorizationURL, body)
	if err != nil {
		return LoginResult{}, err
	}
	verificationURI := clicore.FirstString(clicore.ValueString(device, "verification_uri"), clicore.ValueString(device, "verification_uri_complete"))
	openURL := clicore.FirstString(clicore.ValueString(device, "verification_uri_complete"), verificationURI)
	userCode := clicore.ValueString(device, "user_code")
	openEnabled := clicore.BoolParam(params, true, "open")

	if !clicore.Truthy(clicore.Param(params, "json")) {
		lines := deviceInstructions(device, verificationURI, userCode, !openEnabled)
		for _, line := range lines {
			ioctx.Stdout(line)
		}
		if ioctx.TTY != nil {
			ioctx.TTY("\r\x1b[2K" + strings.Join(lines, "\n") + "\n")
		}
	}

	if ioctx.Phase != nil {
		ioctx.Phase(fmt.Sprintf("Visit %s and enter code %s", verificationURI, userCode))
	}
	if ioctx.Progress != nil {
		ioctx.Progress(0, 1, fmt.Sprintf("Open %s to authorize Putnami Cloud", openURL))
	}

	if openEnabled {
		open := ioctx.OpenBrowser
		if open == nil {
			open = clicore.OpenBrowser
		}
		if err := open(openURL); err == nil && !clicore.Truthy(clicore.Param(params, "json")) {
			ioctx.Stdout("Browser opened automatically.")
			if ioctx.TTY != nil {
				ioctx.TTY("  Browser opened automatically.\n")
			}
		}
	}

	if !clicore.Truthy(clicore.Param(params, "json")) {
		ioctx.Stdout("Waiting for authorization...")
		if ioctx.TTY != nil {
			ioctx.TTY("  Waiting for authorization...\n")
		}
	}

	timeoutMs := clicore.NumberParam(params, "poll-timeout-ms", "pollTimeoutMs")
	if timeoutMs == nil {
		v := clicore.ValueFloat(device, "expires_in")
		if v <= 0 {
			v = 900
		}
		ms := v * 1000
		timeoutMs = &ms
	}
	intervalMs := clicore.NumberParam(params, "poll-interval-ms", "pollIntervalMs")
	if intervalMs == nil {
		v := clicore.ValueFloat(device, "interval")
		if v <= 0 {
			v = 5
		}
		ms := v * 1000
		intervalMs = &ms
	}
	if *intervalMs < 0 {
		*intervalMs = 0
	}

	started := time.Now()
	for {
		if time.Since(started) > time.Duration(*timeoutMs)*time.Millisecond {
			return LoginResult{}, clicore.NewError("device authorization timed out", clicore.ExitAuth)
		}
		response, err := clicore.PostTokenEndpoint(ioctx.Client, endpoints.TokenURL, map[string]any{
			"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
			"device_code": clicore.ValueString(device, "device_code"),
			"client_id":   clientID,
		}, []int{http.StatusOK, http.StatusBadRequest})
		if err != nil {
			return LoginResult{}, err
		}
		if clicore.ValueString(response, "error") == "" {
			auth := clicore.StoredAuth(response, endpoints.Issuer, clientID, ioctx.Now())
			if err := clicore.WriteAuth(auth, env); err != nil {
				return LoginResult{}, err
			}
			claims := clicore.DecodeJWT(auth.AccessToken)
			user := userIdentity(nil, claims)
			if userinfo, err := fetchUserInfo(ioctx.Client, endpoints, auth.AccessToken); err == nil {
				user = userIdentity(userinfo, claims)
			}
			return LoginResult{Auth: auth, User: user, Message: loginSuccessMessage(user)}, nil
		}
		switch clicore.ValueString(response, "error") {
		case "authorization_pending":
			time.Sleep(time.Duration(*intervalMs) * time.Millisecond)
		case "slow_down":
			*intervalMs += clicore.MaxFloat(*intervalMs, 1000)
			time.Sleep(time.Duration(*intervalMs) * time.Millisecond)
		default:
			description := clicore.ValueString(response, "error_description")
			if description == "" {
				description = "authorization failed: " + clicore.ValueString(response, "error")
			}
			return LoginResult{}, clicore.NewError(description, clicore.ExitAuth)
		}
	}
}

// requestDeviceAuthorization starts the device authorization grant: it POSTs
// the client identifier and scope to the issuer's device authorization
// endpoint and returns the device and user codes (RFC 8628 §3.1–§3.2). The
// endpoint is an external contract listed in clientgen.external.json; its URL
// comes from the issuer's discovery document.
func requestDeviceAuthorization(client *http.Client, endpoint string, form map[string]any) (map[string]any, error) {
	body, err := clicore.JSONBody(form)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, body)
	if err != nil {
		return nil, clicore.RequestBuildError(endpoint, err)
	}
	return clicore.SendJSON(client, req, []int{http.StatusOK})
}
