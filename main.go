package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/oauth2"

	"github.com/google/go-containerregistry/pkg/authn"
	ghAuth "github.com/google/go-containerregistry/pkg/authn/github"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-github/v68/github"
	"github.com/pelletier/go-toml"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
		<-sigs
		os.Exit(130)
	}()

	err := run3(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

func run3(ctx context.Context) error {
	var err error
	for _, variant := range []string{"base"} {
		err = buildStack(ctx, variant)
		if err != nil {
			return fmt.Errorf("cannot build (variant: %s): %w", variant, err)
		}
	}
	return err
}

func buildStack(ctx context.Context, variant string) error {
	cli := newGHClient(ctx)
	release, resp, err := cli.Repositories.GetLatestRelease(ctx, "paketo-buildpacks", "jammy-"+variant+"-stack")
	if err != nil {
		return fmt.Errorf("cannot get latest stack version: %w", err)
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	stackArtifacts, err := os.MkdirTemp("", "stack-artifacts-")
	if err != nil {
		return fmt.Errorf("cannot create downloaded OCIs temp dir: %w", err)
	}
	defer func(path string) {
		_ = os.RemoveAll(path)
	}(stackArtifacts)

	downloadedOCIsDir := filepath.Join(stackArtifacts, "downloaded")
	err = os.Mkdir(downloadedOCIsDir, 0744)
	if err != nil {
		return fmt.Errorf("cannot create dir: %w", err)
	}

	var assetsToDownload = make(map[string]*github.ReleaseAsset)
	for _, ra := range release.Assets {
		if ra.GetName() == "jammy-base-stack-"+strings.TrimPrefix(release.GetName(), "v")+"-build.oci" {
			assetsToDownload["build.oci"] = ra
		}
		if ra.GetName() == "jammy-base-stack-"+strings.TrimPrefix(release.GetName(), "v")+"-run.oci" {
			assetsToDownload["run.oci"] = ra
		}
	}

	if len(assetsToDownload) != 2 {
		return fmt.Errorf("missing assets")
	}

	for n, a := range assetsToDownload {
		err = func() error {
			rc, _, err := cli.Repositories.DownloadReleaseAsset(ctx, "paketo-buildpacks", "jammy-"+variant+"-stack", a.GetID(), http.DefaultClient)
			if err != nil {
				return fmt.Errorf("cannot download asset: %w", err)
			}
			defer func(rc io.ReadCloser) {
				_ = rc.Close()
			}(rc)
			f, err := os.OpenFile(filepath.Join(downloadedOCIsDir, n), os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				return fmt.Errorf("cannot create file for downloaded OCI: %w", err)
			}
			defer func(f *os.File) {
				_ = f.Close()
			}(f)
			_, err = io.Copy(f, rc)
			if err != nil {
				return fmt.Errorf("error while saving OCI: %w", err)
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}

	src, err := downloadSource(ctx, release.GetTarballURL())
	if err != nil {
		return fmt.Errorf("canot get stack source code: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(src)
	}()

	err = patchStack(filepath.Join(src, "stack", "stack.toml"))
	if err != nil {
		return fmt.Errorf("cannot patch stack toml: %w", err)
	}

	buildOutputDir := filepath.Join(stackArtifacts, "built")
	err = os.Mkdir(buildOutputDir, 0744)
	if err != nil {
		return fmt.Errorf("cannot create dir: %w", err)
	}

	relp, err := filepath.Rel(src, buildOutputDir)
	if err != nil {
		return fmt.Errorf("cannot get relatipe path for output dir: %w", err)
	}

	cmd := exec.CommandContext(ctx, "scripts/create.sh",
		"--stack-dir", "./stack",
		"--build-dir", relp,
	)
	cmd.Dir = src
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("cannot build stack: %w", err)
	}

	return nil
}

func mergeOciArchives(ociArchiveA, ociArchiveB, destDir string) error {
	extract := func(archivePath string) error {
		return nil
	}
	_ = extract("")
	return nil
}

func downloadSource(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("cannot create http request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot do http get: %w", err)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("badd code: %d", resp.StatusCode)
	}
	d, err := os.MkdirTemp("", "src-")
	if err != nil {
		return "", fmt.Errorf("cannot create temp dir for source code: %w", err)
	}
	cmd := exec.CommandContext(ctx, "tar",
		"xzf", "-",
		"-C", d,
		"--strip-components=1")
	cmd.Stdin = resp.Body
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		return "", fmt.Errorf("cannot extract sources: %w", err)
	}
	return d, nil
}

func newGHClient(ctx context.Context) *github.Client {
	return github.NewClient(oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: os.Getenv("GITHUB_TOKEN"),
	})))
}

func patchStack(stackTomlPath string) error {
	input, err := os.ReadFile(stackTomlPath)
	if err != nil {
		return fmt.Errorf("cannot open stack toml: %w", err)
	}

	var data any
	err = toml.Unmarshal(input, &data)
	if err != nil {
		return fmt.Errorf("cannot decode data: %w", err)
	}

	m := data.(map[string]any)
	m["platforms"] = []string{"linux/amd64", "linux/arm64"}

	args := map[string]interface{}{
		"args": map[string]interface{}{
			"architecture": "arm64",
			"sources": `    deb http://ports.ubuntu.com/ubuntu-ports/ jammy main universe multiverse
    deb http://ports.ubuntu.com/ubuntu-ports/ jammy-updates main universe multiverse
    deb http://ports.ubuntu.com/ubuntu-ports/ jammy-security main universe multiverse
    `},
	}

	m["build"].(map[string]any)["platforms"] = map[string]any{"linux/arm64": args}
	m["run"].(map[string]any)["platforms"] = map[string]any{"linux/arm64": args}

	output, err := toml.Marshal(data)
	err = os.WriteFile(stackTomlPath, output, 0644)
	if err != nil {
		return fmt.Errorf("cannot write patched stack toml: %w", err)
	}

	return nil
}

func pushOciDir(ctx context.Context, ref, ociDirPath string) error {
	path, err := layout.FromPath(ociDirPath)
	if err != nil {
		return err
	}

	imageIndex, err := path.ImageIndex()
	if err != nil {
		return err
	}

	r, err := name.ParseReference(ref)
	if err != nil {
		return err
	}

	return remote.WriteIndex(r, imageIndex,
		remote.WithAuthFromKeychain(DefaultKeychain),
		remote.WithContext(ctx),
	)

}

var DefaultKeychain = authn.NewMultiKeychain(ghAuth.Keychain, authn.DefaultKeychain)
