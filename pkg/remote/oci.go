/*
   Copyright 2020 Docker Compose CLI authors

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

package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/adrg/xdg"
	"github.com/distribution/reference"
	"github.com/docker/buildx/store/storeutil"
	"github.com/docker/buildx/util/imagetools"
	"github.com/docker/cli/cli/command"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/compose-spec/compose-go/loader"
)

func OCIRemoteLoaderEnabled() (bool, error) {
	if v := os.Getenv("COMPOSE_EXPERIMENTAL_OCI_REMOTE"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return false, fmt.Errorf("COMPOSE_EXPERIMENTAL_OCI_REMOTE environment variable expects boolean value: %w", err)
		}
		return enabled, err
	}
	return false, nil
}

func NewOCIRemoteLoader(dockerCli command.Cli, offline bool) (loader.ResourceLoader, error) {
	// xdg.CacheFile creates the parent directories for the target file path
	// and returns the fully qualified path, so use "git" as a filename and
	// then chop it off after, i.e. no ~/.cache/docker-compose/git file will
	// ever be created
	cache, err := xdg.CacheFile(filepath.Join("docker-compose", "oci"))
	if err != nil {
		return nil, fmt.Errorf("initializing git cache: %w", err)
	}
	cache = filepath.Dir(cache)
	return ociRemoteLoader{
		cache:     cache,
		dockerCli: dockerCli,
		offline:   offline,
	}, err
}

type ociRemoteLoader struct {
	cache     string
	dockerCli command.Cli
	offline   bool
}

const prefix = "oci://"

func (g ociRemoteLoader) Accept(path string) bool {
	return strings.HasPrefix(path, prefix)
}

func (g ociRemoteLoader) Load(ctx context.Context, path string) (string, error) {
	if g.offline {
		return "", nil
	}

	ref, err := reference.ParseDockerRef(path[len(prefix):])
	if err != nil {
		return "", err
	}

	opt, err := storeutil.GetImageConfig(g.dockerCli, nil)
	if err != nil {
		return "", err
	}
	resolver := imagetools.New(opt)

	content, descriptor, err := resolver.Get(ctx, ref.String())
	if err != nil {
		return "", err
	}

	local := filepath.Join(g.cache, descriptor.Digest.Hex())
	composeFile := filepath.Join(local, "compose.yaml")
	if _, err = os.Stat(local); os.IsNotExist(err) {

		err = os.MkdirAll(local, 0o700)
		if err != nil {
			return "", err
		}

		f, err := os.Create(composeFile)
		if err != nil {
			return "", err
		}
		defer f.Close() //nolint:errcheck

		var descriptor v1.Manifest
		err = json.Unmarshal(content, &descriptor)
		if err != nil {
			return "", err
		}

		if descriptor.Config.MediaType != "application/vnd.docker.compose.project" {
			return "", fmt.Errorf("%s is not a compose project OCI artifact, but %s", ref.String(), descriptor.Config.MediaType)
		}

		for i, layer := range descriptor.Layers {
			digested, err := reference.WithDigest(ref, layer.Digest)
			if err != nil {
				return "", err
			}
			content, _, err := resolver.Get(ctx, digested.String())
			if err != nil {
				return "", err
			}
			if i > 0 {
				_, err = f.Write([]byte("\n---\n"))
				if err != nil {
					return "", err
				}
			}
			_, err = f.Write(content)
			if err != nil {
				return "", err
			}
		}
	}
	return composeFile, nil
}

var _ loader.ResourceLoader = ociRemoteLoader{}
