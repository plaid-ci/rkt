// Copyright 2014 The rkt Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//+build linux

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema/types"
	"github.com/coreos/rkt/common"
	"github.com/coreos/rkt/stage0"
	"github.com/coreos/rkt/store"
)

var (
	defaultStage1Image string // either set by linker, or guessed in init()

	cmdRun = &Command{
		Name:    "run",
		Summary: "Run image(s) in a pod in rkt",
		Usage:   "[--volume name,kind=host,...] IMAGE [-- image-args...[---]]...",
		Description: `IMAGE should be a string referencing an image; either a hash, local file on disk, or URL.
They will be checked in that order and the first match will be used.

An "--" may be used to inhibit rkt run's parsing of subsequent arguments,
which will instead be appended to the preceding image app's exec arguments.
End the image arguments with a lone "---" to resume argument parsing.`,
		Run:                  runRun,
		Flags:                &runFlags,
		WantsFlagsTerminator: true,
	}
	runFlags        flag.FlagSet
	flagStage1Image string
	flagVolumes     volumeList
	flagPorts       portList
	flagPrivateNet  bool
	flagInheritEnv  bool
	flagExplicitEnv envMap
	flagInteractive bool
	flagNoOverlay   bool
	flagLocal       bool
	flagPodManifest string
)

func init() {
	commands = append(commands, cmdRun)

	// if not set by linker, try discover the directory rkt is running
	// from, and assume the default stage1.aci is stored alongside it.
	if defaultStage1Image == "" {
		if exePath, err := os.Readlink("/proc/self/exe"); err == nil {
			defaultStage1Image = filepath.Join(filepath.Dir(exePath), "stage1.aci")
		}
	}

	runFlags.StringVar(&flagStage1Image, "stage1-image", defaultStage1Image, `image to use as stage1. Local paths and http/https URLs are supported. If empty, rkt will look for a file called "stage1.aci" in the same directory as rkt itself`)
	runFlags.Var(&flagVolumes, "volume", "volumes to mount into the pod")
	runFlags.Var(&flagPorts, "port", "ports to expose on the host (requires --private-net)")
	runFlags.BoolVar(&flagPrivateNet, "private-net", false, "give pod a private network")
	runFlags.BoolVar(&flagInheritEnv, "inherit-env", false, "inherit all environment variables not set by apps")
	runFlags.BoolVar(&flagNoOverlay, "no-overlay", false, "disable overlay filesystem")
	runFlags.Var(&flagExplicitEnv, "set-env", "an environment variable to set for apps in the form name=value")
	runFlags.BoolVar(&flagInteractive, "interactive", false, "run pod interactively")
	runFlags.Var((*appAsc)(&rktApps), "signature", "local signature file to use in validating the preceding image")
	runFlags.BoolVar(&flagLocal, "local", false, "use only local images (do not discover or download from remote URLs)")
	runFlags.StringVar(&flagPodManifest, "pod-manifest", "", "the path to the pod manifest. If it's non-empty, then only '--private-net', '--no-overlay' and '--interactive' will have effects")
	flagVolumes = volumeList{}
	flagPorts = portList{}
}

func runRun(args []string) (exit int) {
	if len(flagPorts) > 0 && !flagPrivateNet {
		stderr("--port flag requires --private-net")
		return 1
	}

	if len(flagPodManifest) > 0 && (len(flagVolumes) > 0 || len(flagPorts) > 0 || flagInheritEnv || !flagExplicitEnv.IsEmpty() || rktApps.Count() > 0 || flagLocal) {
		stderr("conflicting flags set with --pod-manifest (see --help)")
		return 1
	}

	if globalFlags.Dir == "" {
		log.Printf("dir unset - using temporary directory")
		var err error
		globalFlags.Dir, err = ioutil.TempDir("", "rkt")
		if err != nil {
			stderr("error creating temporary directory: %v", err)
			return 1
		}
	}

	err := parseApps(&rktApps, args, &runFlags, true)
	if err != nil {
		stderr("run: error parsing app image arguments: %v", err)
		return 1
	}

	if flagInteractive && rktApps.Count() > 1 {
		stderr("run: interactive option only supports one image")
		return 1
	}

	if rktApps.Count() < 1 && len(flagPodManifest) == 0 {
		stderr("run: must provide at least one image or specify the pod manifest")
		return 1
	}

	s, err := store.NewStore(globalFlags.Dir)
	if err != nil {
		stderr("run: cannot open store: %v", err)
		return 1
	}

	config, err := getConfig()
	if err != nil {
		stderr("run: cannot get configuration: %v", err)
		return 1
	}
	fn := &finder{
		imageActionData: imageActionData{
			s:                  s,
			headers:            config.AuthPerHost,
			dockerAuth:         config.DockerCredentialsPerRegistry,
			insecureSkipVerify: globalFlags.InsecureSkipVerify,
			debug:              globalFlags.Debug,
		},
		local:    flagLocal,
		withDeps: false,
	}
	s1img, err := fn.findImage(flagStage1Image, "", false)
	if err != nil {
		stderr("Error finding stage1 image %q: %v", flagStage1Image, err)
		return 1
	}

	fn.ks = getKeystore()
	fn.withDeps = true
	if err := fn.findImages(&rktApps); err != nil {
		stderr("%v", err)
		return 1
	}

	p, err := newPod()
	if err != nil {
		stderr("Error creating new pod: %v", err)
		return 1
	}

	cfg := stage0.CommonConfig{
		Store:       s,
		Stage1Image: *s1img,
		UUID:        p.uuid,
		Debug:       globalFlags.Debug,
	}

	pcfg := stage0.PrepareConfig{
		CommonConfig: cfg,
		UseOverlay:   !flagNoOverlay && common.SupportsOverlay(),
	}

	if len(flagPodManifest) > 0 {
		pcfg.PodManifest = flagPodManifest
	} else {
		pcfg.Volumes = []types.Volume(flagVolumes)
		pcfg.Ports = []types.ExposedPort(flagPorts)
		pcfg.InheritEnv = flagInheritEnv
		pcfg.ExplicitEnv = flagExplicitEnv.Strings()
		pcfg.Apps = &rktApps
	}

	err = stage0.Prepare(pcfg, p.path(), p.uuid)
	if err != nil {
		stderr("run: error setting up stage0: %v", err)
		return 1
	}

	// get the lock fd for run
	lfd, err := p.Fd()
	if err != nil {
		stderr("Error getting pod lock fd: %v", err)
		return 1
	}

	// skip prepared by jumping directly to run, we own this pod
	if err := p.xToRun(); err != nil {
		stderr("run: unable to transition to run: %v", err)
		return 1
	}

	rcfg := stage0.RunConfig{
		CommonConfig: cfg,
		PrivateNet:   flagPrivateNet,
		LockFd:       lfd,
		Interactive:  flagInteractive,
		Images:       rktApps.GetImageIDs(),
	}
	stage0.Run(rcfg, p.path()) // execs, never returns

	return 1
}

// volumeList implements the flag.Value interface to contain a set of mappings
// from mount label --> mount path
type volumeList []types.Volume

func (vl *volumeList) Set(s string) error {
	vol, err := types.VolumeFromString(s)
	if err != nil {
		return err
	}

	*vl = append(*vl, *vol)
	return nil
}

func (vl *volumeList) String() string {
	var vs []string
	for _, v := range []types.Volume(*vl) {
		vs = append(vs, v.String())
	}
	return strings.Join(vs, " ")
}

// portList implements the flag.Value interface to contain a set of mappings
// from port name --> host port
type portList []types.ExposedPort

func (pl *portList) Set(s string) error {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("%q is not in name:port format", s)
	}

	name, err := types.NewACName(parts[0])
	if err != nil {
		return fmt.Errorf("%q is not a valid port name: %v", parts[0], err)
	}

	port, err := strconv.ParseUint(parts[1], 10, 16)
	if err != nil {
		return fmt.Errorf("%q is not a valid port number", parts[1])
	}

	p := types.ExposedPort{
		Name:     *name,
		HostPort: uint(port),
	}

	*pl = append(*pl, p)
	return nil
}

func (pl *portList) String() string {
	var ps []string
	for _, p := range []types.ExposedPort(*pl) {
		ps = append(ps, fmt.Sprintf("%v:%v", p.Name, p.HostPort))
	}
	return strings.Join(ps, " ")
}

// envMap implements the flag.Value interface to contain a set of name=value mappings
type envMap struct {
	mapping map[string]string
}

func (e *envMap) Set(s string) error {
	if e.mapping == nil {
		e.mapping = make(map[string]string)
	}
	pair := strings.SplitN(s, "=", 2)
	if len(pair) != 2 {
		return fmt.Errorf("environment variable must be specified as name=value")
	}
	if _, exists := e.mapping[pair[0]]; exists {
		return fmt.Errorf("environment variable %q already set", pair[0])
	}
	e.mapping[pair[0]] = pair[1]
	return nil
}

func (e *envMap) IsEmpty() bool {
	return len(e.mapping) == 0
}

func (e *envMap) String() string {
	return strings.Join(e.Strings(), "\n")
}

func (e *envMap) Strings() []string {
	var env []string
	for n, v := range e.mapping {
		env = append(env, n+"="+v)
	}
	return env
}
