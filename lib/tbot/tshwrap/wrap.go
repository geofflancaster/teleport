/*
Copyright 2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tshwrap

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/coreos/go-semver/semver"
	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/constants"
	"github.com/gravitational/teleport/api/identityfile"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/tbot/config"
	"github.com/gravitational/teleport/lib/tbot/identity"
	"github.com/gravitational/teleport/lib/tlsca"
)

const (
	// TSHVarName is the name of the environment variable that can override the
	// tsh path that would otherwise be located on the $PATH.
	TSHVarName = "TSH"

	// TSHMinVersion is the minimum version of tsh that supports Machine ID
	// proxies.
	TSHMinVersion = "9.3.0"
)

var log = logrus.WithFields(logrus.Fields{
	trace.Component: teleport.ComponentTBot,
})

// capture runs a command (presumably tsh) with the given arguments and
// returns it's captured stdout. Stderr is ignored. Errors are returned per
// exec.Command().Output() semantics.
func capture(tshPath string, args ...string) ([]byte, error) {
	out, err := exec.Command(tshPath, args...).Output()
	if err != nil {
		return nil, trace.Wrap(err, "error executing tsh")
	}

	return out, nil
}

// Wrapper is a wrapper to execute `tsh` commands via a subprocess.
type Wrapper struct {
	// path is a path to the tsh executable
	path string

	// capture is the function for capturing a command's output. It may be
	// overridden by tests for mocking purposes, but by default is expected to
	// execute an actual tsh binary on the host system.
	capture func(tshPath string, args ...string) ([]byte, error)
}

// New creates a new tsh wrapper. If a $TSH var is set it uses that path,
// otherwise looks for tsh on the OS path.
func New() (*Wrapper, error) {
	if val, ok := os.LookupEnv(TSHVarName); ok {
		return &Wrapper{
			path:    val,
			capture: capture,
		}, nil
	}

	binary := "tsh"
	if runtime.GOOS == constants.WindowsOS {
		binary = "tsh.exe"
	}

	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &Wrapper{
		path:    path,
		capture: capture,
	}, nil
}

// Exec runs tsh with the given environment variables and arguments. The child
// process inherits stdin/stdout/stderr and runs until completion. Errors are
// returned per `exec.Command().Run()` semantics.
func (w *Wrapper) Exec(env map[string]string, args ...string) error {
	// The subprocess should inherit the environment plus our vars. Our env
	// vars will safely overwrite those from the environment, per `exec.Cmd`
	// docs.
	environ := os.Environ()
	for k, v := range env {
		// In case of similar keys, last env var wins.
		environ = append(environ, k+"="+v)
	}

	log.Debugf("executing %s with env=%+v and args=%+v", w.path, env, args)

	child := exec.Command(w.path, args...)
	child.Env = environ
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr

	return trace.Wrap(child.Run(), "unable to execute tsh")
}

// GetTSHVersion queries the system tsh for its version.
func GetTSHVersion(w *Wrapper) (*semver.Version, error) {
	rawVersion, err := w.capture(w.path, "version", "-f", "json")
	if err != nil {
		return nil, trace.Wrap(err, "querying tsh version")
	}

	versionInfo := struct {
		Version string `json:"version"`
	}{}
	if err := json.Unmarshal(rawVersion, &versionInfo); err != nil {
		return nil, trace.Wrap(err, "error deserializing tsh version from string: %s", rawVersion)
	}

	sv, err := semver.NewVersion(versionInfo.Version)
	if err != nil {
		return nil, trace.Wrap(err, "error parsing tsh version: %s", versionInfo.Version)
	}

	return sv, nil
}

// CheckTSHSupported checks if the current tsh supports Machine ID.
func CheckTSHSupported(w *Wrapper) error {
	version, err := GetTSHVersion(w)
	if err != nil {
		return trace.Wrap(err, "unable to determine tsh version")
	}

	minVersion := semver.New(TSHMinVersion)
	if version.LessThan(*minVersion) {
		return trace.BadParameter(
			"installed tsh version %s does not support Machine ID proxies, "+
				"please upgrade to at least %s",
			version, minVersion,
		)
	}

	log.Debugf("tsh version %s is supported", version)

	return nil
}

func GetAllClusters(w *Wrapper, identityFilePath string, proxy string) ([]string, error) {
	rawVersion, err := w.capture(w.path, "-i", identityFilePath, "--proxy", proxy, "kube", "ls", "-f", "json")
	if err != nil {
		return nil, trace.Wrap(err, "querying tsh version")
	}

	allClusters := []struct {
		ClusterName string `json:"kube_cluster_name"`
	}{}
	if err := json.Unmarshal(rawVersion, &allClusters); err != nil {
		return nil, trace.Wrap(err, "error deserializing tsh version from string: %s", rawVersion)
	}

	var clusterNames []string
	for _, c := range allClusters {
		clusterNames = append(clusterNames, c.ClusterName)
	}

	return clusterNames, nil
}

// GetDestinationDirectory attempts to select an unambiguous destination, either from
// CLI or YAML config. It returns an error if the selected destination is
// invalid.
func GetDestinationDirectory(botConfig *config.BotConfig) (*config.DestinationDirectory, error) {
	// WARNING:
	// This code is dependent on some unexpected "behavior" in
	// config.FromCLIConf() - when users provide --destination-dir then all
	// outputs configured in the YAML file are overwritten by an identity
	// output with a directory destination with a path of --destination-dir.
	// See: https://github.com/gravitational/teleport/issues/27206
	if len(botConfig.Outputs) == 0 {
		return nil, trace.BadParameter("either --destination-dir or a config file containing an output must be specified")
	} else if len(botConfig.Outputs) > 1 {
		return nil, trace.BadParameter("the config file contains multiple outputs; a --destination-dir must be specified")
	}
	destination := botConfig.Outputs[0].GetDestination()
	destinationDir, ok := destination.(*config.DestinationDirectory)
	if !ok {
		return nil, trace.BadParameter("destination %s must be a directory", destination)
	}

	return destinationDir, nil
}

// mergeEnv applies the given value to each key inside the specified map.
func mergeEnv(m map[string]string, value string, keys []string) {
	for _, key := range keys {
		m[key] = value
	}
}

// GetEnvForTSH returns a map of environment variables needed to properly wrap
// tsh so that it uses our Machine ID certificates where necessary.
func GetEnvForTSH(destPath string) (map[string]string, error) {
	// The env var interface does allow us to set specific resource names for
	// everything but also has generic fallbacks. We'll use the fallbacks for
	// now but could eventually communicate more info to tsh if desired.
	env := make(map[string]string)
	mergeEnv(env, filepath.Join(destPath, identity.PrivateKeyKey), client.VirtualPathEnvNames(client.VirtualPathKey, nil))

	// Database certs are a bit awkward since a few databases (cockroach) have
	// special naming requirements. We can document around these for now and
	// automate later. (I don't think tsh handles this perfectly today anyway).
	mergeEnv(env, filepath.Join(destPath, identity.TLSCertKey), client.VirtualPathEnvNames(client.VirtualPathDatabase, nil))

	mergeEnv(env, filepath.Join(destPath, identity.TLSCertKey), client.VirtualPathEnvNames(client.VirtualPathApp, nil))

	// We don't want to provide a fallback for CAs since it would be ambiguous,
	// so we'll specify them exactly.
	env[client.VirtualPathEnvName(client.VirtualPathCA, client.VirtualPathCAParams(types.UserCA))] = filepath.Join(destPath, config.UserCAPath)
	env[client.VirtualPathEnvName(client.VirtualPathCA, client.VirtualPathCAParams(types.HostCA))] = filepath.Join(destPath, config.HostCAPath)
	env[client.VirtualPathEnvName(client.VirtualPathCA, client.VirtualPathCAParams(types.DatabaseCA))] = filepath.Join(destPath, config.DatabaseCAPath)

	// TODO(timothyb89): Kubernetes support. We don't generate kubeconfigs yet, so we have
	// nothing to give tsh for now.

	return env, nil
}

// LoadIdentity loads a Teleport identity from an identityfile. Secondary bot
// identities are not loadable, so we'll just read the Teleport identity (which
// is required for tsh to function anyway).
func LoadIdentity(identityPath string) (*tlsca.Identity, error) {
	f, err := os.Open(identityPath)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer f.Close()

	idFile, err := identityfile.Read(f)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	cert, err := tlsca.ParseCertificatePEM(idFile.Certs.TLS)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	parsed, err := tlsca.FromSubject(cert.Subject, cert.NotAfter)
	return parsed, trace.Wrap(err)
}
