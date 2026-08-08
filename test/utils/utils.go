/*
Copyright 2026.

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

package utils

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
)

// Run executes cmd from the project root. An environment already set on cmd is
// kept: a command built by Cluster.Command carries the pinned KUBECONFIG there,
// and replacing it would hand the command back to the ambient environment.
//
// A kubectl or helm invocation that was not built by Cluster.Command is
// refused rather than executed: it would resolve its target from the ambient
// environment, which is how a run reaches a cluster it did not create.
func Run(cmd *exec.Cmd) (string, error) {
	if len(cmd.Args) > 0 {
		if err := requirePinned(cmd.Args[0], cmd.Args[1:]); err != nil {
			return "", err
		}
	}

	if cmd.Dir == "" {
		dir, err := GetProjectDir()
		if err != nil {
			return "", err
		}
		cmd.Dir = dir
	}

	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = append(env, "GO111MODULE=on")

	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// requirePinned reports an invocation that talks to an API server without
// naming the kubeconfig it should use. Only Cluster.Command produces that flag,
// so this is what keeps a new call site from quietly inheriting the ambient
// context.
func requirePinned(name string, args []string) error {
	if !IsClusterCommand(name, args) {
		return nil
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--kubeconfig=") {
			return nil
		}
	}
	return fmt.Errorf("refusing to run %s: it is not pinned to a kubeconfig, build it with Cluster.Command",
		binaryName(name))
}

// LoadImageToKindCluster side-loads a locally built image into one named kind
// cluster. The name is required: kind's own default is the cluster literally
// called "kind", which is never the cluster an e2e run created.
func LoadImageToKindCluster(kindBin, cluster, image string) error {
	if cluster == "" {
		return fmt.Errorf("no kind cluster name given; refusing to load %s into kind's default cluster", image)
	}
	if kindBin == "" {
		kindBin = "kind"
	}
	_, err := Run(exec.Command(kindBin, "load", "docker-image", image, "--name", cluster))
	return err
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %q to be uncomment", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		if _, err = out.WriteString(strings.TrimPrefix(scanner.Text(), prefix)); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err = out.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
	}

	if _, err = out.Write(content[idx+len(target):]); err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	// false positive
	// nolint:gosec
	if err = os.WriteFile(filename, out.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write file %q: %w", filename, err)
	}

	return nil
}
