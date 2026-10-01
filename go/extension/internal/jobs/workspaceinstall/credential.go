package workspaceinstall

import (
	"errors"
	"fmt"
	"os"
	"strings"

	registry "go.putnami.dev/protocol/registry"
)

// netrcLogin is the login of every entry of the ephemeral NETRC. Go sends a
// NETRC entry as HTTP Basic credentials; the registry reads the password as
// the bearer and ignores the login, so the login is the conventional
// placeholder the native credential uses too.
const netrcLogin = "_token"

// withCredential runs fn with env plus, when the engine handed the job a read
// credential, NETRC naming an ephemeral file that holds it. The file exists
// only while fn runs: it is written before, and it and its directory are
// removed after, whether fn succeeds or fails, and when a signal ends the job
// before fn returns (workspacejob.Trap.OnSignal).
//
// fn must start only go commands that download modules: they are the one
// process the credential is for, and the only one that learns where the file
// is. The bearer itself is never in an environment variable, of this process
// or of any child.
//
// Without a credential fn runs with env unchanged, so go reads the machine's
// own credentials as it does on every other run.
func (w *install) withCredential(env []string, fn func(env []string) bool) bool {
	if w.credential == nil {
		return fn(env)
	}
	path, remove, err := writeCredentialNetrc(w.credential)
	if err != nil {
		w.Emit.Diagnostic("error", "Cannot write the registry credential for the module download: "+err.Error(), "", 0)
		return false
	}
	release := w.Trap().OnSignal(remove)
	defer func() {
		remove()
		release()
	}()
	// GOAUTH=netrc because a GOAUTH from the machine that leaves netrc out
	// makes go ignore the file, and one that names a command would run it.
	return fn(append(append([]string(nil), env...), "NETRC="+path, "GOAUTH=netrc"))
}

// writeCredentialNetrc writes credential as a NETRC file only this user can
// read (os.CreateTemp creates it 0600), in a directory only this user can
// enter (os.MkdirTemp creates it 0700), and returns its path and the function
// that removes the file and the directory. remove may run more than once.
//
// It writes one entry per host the credential serves, in the host form go
// matches: go looks an entry up by the request URL without its scheme, host
// and port included, so a host with a port is written with it, and the default
// https port, 443, which a URL leaves out, is also written without it.
func writeCredentialNetrc(credential *registry.Credential) (path string, remove func(), err error) {
	content, err := credentialNetrc(credential)
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "putnami-go-netrc-")
	if err != nil {
		return "", nil, err
	}
	remove = func() { _ = os.RemoveAll(dir) }
	file, err := os.CreateTemp(dir, "netrc-")
	if err != nil {
		remove()
		return "", nil, err
	}
	_, writeErr := file.WriteString(content)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		remove()
		return "", nil, err
	}
	return file.Name(), remove, nil
}

// credentialNetrc renders the NETRC entries of credential. A host or a bearer
// that is not well formed is refused rather than written: either could split
// the file into entries the provider did not grant.
func credentialNetrc(credential *registry.Credential) (string, error) {
	if credential == nil || len(credential.Hosts) == 0 {
		return "", errors.New("the credential serves no host")
	}
	if !registry.ValidCredentialBearer(credential.Bearer) {
		return "", errors.New("the credential bearer is not well formed")
	}
	var content strings.Builder
	written := make(map[string]bool, len(credential.Hosts))
	for _, host := range credential.Hosts {
		if !registry.ValidCredentialHost(host) {
			return "", fmt.Errorf("the credential host %q is not well formed", host)
		}
		machines := []string{host}
		if name, port, ok := strings.Cut(host, ":"); ok && port == "443" {
			machines = append(machines, name)
		}
		for _, machine := range machines {
			if written[machine] {
				continue
			}
			written[machine] = true
			content.WriteString("machine " + machine + " login " + netrcLogin + " password " + credential.Bearer + "\n")
		}
	}
	return content.String(), nil
}
