package build

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/drone/drone/pkg/build/buildfile"
	"github.com/drone/drone/pkg/build/docker"
	"github.com/drone/drone/pkg/build/dockerfile"
	"github.com/drone/drone/pkg/build/log"
	"github.com/drone/drone/pkg/build/proxy"
	"github.com/drone/drone/pkg/build/repo"
	"github.com/drone/drone/pkg/build/script"
)

// BuildState stores information about a build
// process including the Exit status and various
// Runtime statistics (coming soon).
type BuildState struct {
	Started  int64
	Finished int64
	ExitCode int

	// we may eventually include detailed resource
	// usage statistics, including including CPU time,
	// Max RAM, Max Swap, Disk space, and more.
}

func New(dockerClient *docker.Client) *Builder {
	return &Builder{
		dockerClient: dockerClient,
	}
}

// Builder represents a build process being prepared
// to run.
type Builder struct {
	// Image specifies the Docker Image that will be
	// used to virtualize the Build process.
	Build *script.Build

	// Source specifies the Repository path of the code
	// that we are testing.
	//
	// The source repository may be a local repository
	// on the current filesystem, or a remote repository
	// on GitHub, Bitbucket, etc.
	Repo *repo.Repo

	// Key is an identify file, such as an RSA private key, that
	// will be copied into the environments ~/.ssh/id_rsa file.
	Key []byte

	// Timeout is the maximum amount of to will wait for a process
	// to exit.
	//
	// The default is no timeout.
	Timeout time.Duration

	// Stdout specifies the builds's standard output.
	//
	// If stdout is nil, Run connects the corresponding file descriptor
	// to the null device (os.DevNull).
	Stdout io.Writer

	// BuildState contains information about an exited build,
	// available after a call to Run.
	BuildState *BuildState

	// Docker image that was created for
	// this build.
	image *docker.Image

	// Docker container was that created
	// for this build.
	container *docker.Run

	// Docker containers created for the
	// specified services and linked to
	// this build.
	services []*docker.Container

	dockerClient *docker.Client
}

func (b *Builder) Run() error {
	// teardown will remove the Image and stop and
	// remove the service containers after the
	// build is done running.
	defer b.teardown()

	// setup will create the Image and supporting
	// service containers.
	if err := b.setup(); err != nil {
		return err
	}

	// make sure build state is not nil
	b.BuildState = &BuildState{}
	b.BuildState.ExitCode = 0
	b.BuildState.Started = time.Now().UTC().Unix()

	c := make(chan error, 1)
	go func() {
		c <- b.run()
	}()

	// wait for either a) the job to complete or b) the job to timeout
	select {
	case err := <-c:
		return err
	case <-time.After(b.Timeout):
		log.Errf("time limit exceeded for build %s", b.Build.Name)
		b.BuildState.ExitCode = 124
		b.BuildState.Finished = time.Now().UTC().Unix()
		return nil
	}
}

func (b *Builder) setup() error {

	// temp directory to store all files required
	// to generate the Docker image.
	dir, err := ioutil.TempDir("", "drone-")
	if err != nil {
		return err
	}

	// clean up after our mess.
	defer os.RemoveAll(dir)

	// make sure the image isn't empty. this would be bad
	if len(b.Build.Image) == 0 {
		log.Err("Fatal Error, No Docker Image specified")
		return fmt.Errorf("Error: missing Docker image")
	}

	// if we're using an alias for the build name we
	// should substitute it now
	if alias, ok := builders[b.Build.Image]; ok {
		b.Build.Image = alias.Tag
	}

	// if this is a local repository we should symlink
	// to the source code in our temp directory
	if b.Repo.IsLocal() {
		// this is where we used to use symlinks. We should
		// talk to the docker team about this, since copying
		// the entire repository is slow :(
		//
		// see https://github.com/dotcloud/docker/pull/3567

		//src := filepath.Join(dir, "src")
		//err = os.Symlink(b.Repo.Path, src)
		//if err != nil {
		//	return err
		//}

		src := filepath.Join(dir, "src")
		cmd := exec.Command("cp", "-a", b.Repo.Path, src)
		if err := cmd.Run(); err != nil {
			return err
		}
	}

	// start all services required for the build
	// that will get linked to the container.
	for i, service := range b.Build.Services {
		image, err := getImage(service)
		if err != nil {
			return err
		}

		// debugging
		log.Infof("starting service container %s", b.Build.Services[i])

		// Run the contianer
		run, err := b.dockerClient.Containers.RunDaemonPorts(image.Tag, image.Ports...)
		if err != nil {
			return err
		}

		// Get the container info
		info, err := b.dockerClient.Containers.Inspect(run.ID)
		if err != nil {
			// on error kill the container since it hasn't yet been
			// added to the array and would therefore not get
			// removed in the defer statement.
			b.dockerClient.Containers.Stop(run.ID, 10)
			b.dockerClient.Containers.Remove(run.ID)
			return err
		}

		// Add the running service to the list
		b.services = append(b.services, info)

	}

	if err := b.writeIdentifyFile(dir); err != nil {
		return err
	}

	if err := b.writeBuildScript(dir); err != nil {
		return err
	}

	if err := b.writeProxyScript(dir); err != nil {
		return err
	}

	if err := b.writeDockerfile(dir); err != nil {
		return err
	}

	// debugging
	log.Info("creating build image")

	// check for build container (ie bradrydzewski/go:1.2)
	// and download if it doesn't already exist
	if _, err := b.dockerClient.Images.Inspect(b.Build.Image); err == docker.ErrNotFound {
		// download the image if it doesn't exist
		if err := b.dockerClient.Images.Pull(b.Build.Image); err != nil {
			return err
		}
	}

	// create the Docker image
	id := createUID()
	if err := b.dockerClient.Images.Build(id, dir); err != nil {
		return err
	}

	// debugging
	log.Infof("copying repository to %s", b.Repo.Dir)

	// get the image details
	b.image, err = b.dockerClient.Images.Inspect(id)
	if err != nil {
		// if we have problems with the image make sure
		// we remove it before we exit
		b.dockerClient.Images.Remove(id)
		return err
	}

	return nil
}

// teardown is a helper function that we can use to
// stop and remove the build container, its supporting image,
// and the supporting service containers.
func (b *Builder) teardown() error {

	// stop and destroy the container
	if b.container != nil {

		// debugging
		log.Info("removing build container")

		// stop the container, ignore error message
		b.dockerClient.Containers.Stop(b.container.ID, 15)

		// remove the container, ignore error message
		if err := b.dockerClient.Containers.Remove(b.container.ID); err != nil {
			log.Errf("failed to delete build container %s", b.container.ID)
		}
	}

	// stop and destroy the container services
	for i, container := range b.services {
		// debugging
		log.Infof("removing service container %s", b.Build.Services[i])

		// stop the service container, ignore the error
		b.dockerClient.Containers.Stop(container.ID, 15)

		// remove the service container, ignore the error
		if err := b.dockerClient.Containers.Remove(container.ID); err != nil {
			log.Errf("failed to delete service container %s", container.ID)
		}
	}

	// destroy the underlying image
	if b.image != nil {
		// debugging
		log.Info("removing build image")

		if _, err := b.dockerClient.Images.Remove(b.image.ID); err != nil {
			log.Errf("failed to completely delete build image %s. %s", b.image.ID, err.Error())
		}
	}

	return nil
}

func (b *Builder) run() error {
	// create and run the container
	conf := docker.Config{
		Image:        b.image.ID,
		AttachStdin:  false,
		AttachStdout: true,
		AttachStderr: true,
	}
	host := docker.HostConfig{
		Privileged: false,
	}

	// debugging
	log.Noticef("starting build %s", b.Build.Name)

	// link service containers
	for i, service := range b.services {
		image, err := getImage(b.Build.Services[i])
		if err != nil {
			return err
		}
		// link the service container to our
		// build container.
		host.Links = append(host.Links, service.Name[1:]+":"+image.Name)
	}

	// where are temp files going to go?
	tmp_path := "/tmp/drone"
	if len(os.Getenv("DRONE_TMP")) > 0 {
		tmp_path = os.Getenv("DRONE_TMP")
	}

	log.Infof("temp directory is %s", tmp_path)

	if err := os.MkdirAll(tmp_path, 0777); err != nil {
		return fmt.Errorf("Failed to create temp directory at %s: %s", tmp_path, err)
	}

	// link cached volumes
	conf.Volumes = make(map[string]struct{})
	for _, volume := range b.Build.Cache {
		name := filepath.Clean(b.Repo.Name)
		branch := filepath.Clean(b.Repo.Branch)
		volume := filepath.Clean(volume)

		// with Docker, volumes must be an absolute path. If an absolute
		// path is not provided, then assume it is for the repository
		// working directory.
		if strings.HasPrefix(volume, "/") == false {
			volume = filepath.Join(b.Repo.Dir, volume)
		}

		// local cache path on the host machine
		// this path is going to be really long
		hostpath := filepath.Join(tmp_path, name, branch, volume)

		// check if the volume is created
		if _, err := os.Stat(hostpath); err != nil {
			// if does not exist then create
			os.MkdirAll(hostpath, 0777)
		}

		host.Binds = append(host.Binds, hostpath+":"+volume)
		conf.Volumes[volume] = struct{}{}

		// debugging
		log.Infof("mounting volume %s:%s", hostpath, volume)
	}

	// create the container from the image
	run, err := b.dockerClient.Containers.Create(&conf)
	if err != nil {
		return err
	}

	// cache instance of docker.Run
	b.container = run

	// attach to the container
	go func() {
		b.dockerClient.Containers.Attach(run.ID, &writer{b.Stdout})
	}()

	// start the container
	if err := b.dockerClient.Containers.Start(run.ID, &host); err != nil {
		b.BuildState.ExitCode = 1
		b.BuildState.Finished = time.Now().UTC().Unix()
		return err
	}

	// wait for the container to stop
	wait, err := b.dockerClient.Containers.Wait(run.ID)
	if err != nil {
		b.BuildState.ExitCode = 1
		b.BuildState.Finished = time.Now().UTC().Unix()
		return err
	}

	// set completion time
	b.BuildState.Finished = time.Now().UTC().Unix()

	// get the exit code if possible
	b.BuildState.ExitCode = wait.StatusCode

	return nil
}

// writeDockerfile is a helper function that generates a
// Dockerfile and writes to the builds temporary directory
// so that it can be used to create the Image.
func (b *Builder) writeDockerfile(dir string) error {
	var dockerfile = dockerfile.New(b.Build.Image)
	dockerfile.WriteWorkdir(b.Repo.Dir)
	dockerfile.WriteAdd("drone", "/usr/local/bin/")

	// upload source code if repository is stored
	// on the host machine
	if b.Repo.IsRemote() == false {
		dockerfile.WriteAdd("src", filepath.Join(b.Repo.Dir))
	}

	switch {
	case strings.HasPrefix(b.Build.Image, "bradrydzewski/"),
		strings.HasPrefix(b.Build.Image, "drone/"):
		// the default user for all official Drone imnage
		// is the "ubuntu" user, since all build images
		// inherit from the ubuntu cloud ISO
		dockerfile.WriteUser("ubuntu")
		dockerfile.WriteEnv("HOME", "/home/ubuntu")
		dockerfile.WriteEnv("LANG", "en_US.UTF-8")
		dockerfile.WriteEnv("LANGUAGE", "en_US:en")
		dockerfile.WriteEnv("LOGNAME", "ubuntu")
		dockerfile.WriteEnv("TERM", "xterm")
		dockerfile.WriteEnv("SHELL", "/bin/bash")
		dockerfile.WriteAdd("id_rsa", "/home/ubuntu/.ssh/id_rsa")
		dockerfile.WriteRun("sudo chown -R ubuntu:ubuntu /home/ubuntu/.ssh")
		dockerfile.WriteRun("sudo chown -R ubuntu:ubuntu /var/cache/drone")
		dockerfile.WriteRun("sudo chown -R ubuntu:ubuntu /usr/local/bin/drone")
		dockerfile.WriteRun("sudo chmod 600 /home/ubuntu/.ssh/id_rsa")
	default:
		// all other images are assumed to use
		// the root user.
		dockerfile.WriteUser("root")
		dockerfile.WriteEnv("HOME", "/root")
		dockerfile.WriteEnv("LANG", "en_US.UTF-8")
		dockerfile.WriteEnv("LANGUAGE", "en_US:en")
		dockerfile.WriteEnv("LOGNAME", "root")
		dockerfile.WriteEnv("TERM", "xterm")
		dockerfile.WriteEnv("SHELL", "/bin/bash")
		dockerfile.WriteEnv("GOPATH", "/var/cache/drone")
		dockerfile.WriteAdd("id_rsa", "/root/.ssh/id_rsa")
		dockerfile.WriteRun("chmod 600 /root/.ssh/id_rsa")
		dockerfile.WriteRun("echo 'StrictHostKeyChecking no' > /root/.ssh/config")
	}

	dockerfile.WriteAdd("proxy.sh", "/etc/drone.d/")
	dockerfile.WriteEntrypoint("/bin/bash -e /usr/local/bin/drone")

	// write the Dockerfile to the temporary directory
	return ioutil.WriteFile(filepath.Join(dir, "Dockerfile"), dockerfile.Bytes(), 0700)
}

// writeBuildScript is a helper function that
// will generate the build script file in the builder's
// temp directory to be added to the Image.
func (b *Builder) writeBuildScript(dir string) error {
	f := buildfile.New()

	// add environment variables about the build
	f.WriteEnv("CI", "true")
	f.WriteEnv("DRONE", "true")
	f.WriteEnv("DRONE_BRANCH", b.Repo.Branch)
	f.WriteEnv("DRONE_COMMIT", b.Repo.Commit)
	f.WriteEnv("DRONE_PR", b.Repo.PR)
	f.WriteEnv("DRONE_BUILD_DIR", b.Repo.Dir)

	// add /etc/hosts entries
	for _, mapping := range b.Build.Hosts {
		f.WriteHost(mapping)
	}

	// if the repository is remote then we should
	// add the commands to the build script to
	// clone the repository
	if b.Repo.IsRemote() {
		for _, cmd := range b.Repo.Commands() {
			f.WriteCmd(cmd)
		}
	}

	// if the commit is for merging a pull request
	// we should only execute the build commands,
	// and omit the deploy and publish commands.
	if len(b.Repo.PR) == 0 {
		b.Build.Write(f)
	} else {
		// only write the build commands
		b.Build.WriteBuild(f)
	}

	scriptfilePath := filepath.Join(dir, "drone")
	return ioutil.WriteFile(scriptfilePath, f.Bytes(), 0700)
}

// writeProxyScript is a helper function that
// will generate the proxy.sh file in the builder's
// temp directory to be added to the Image.
func (b *Builder) writeProxyScript(dir string) error {
	var proxyfile = proxy.Proxy{}

	// loop through services so that we can
	// map ip address to localhost
	for _, container := range b.services {
		// create an entry for each port
		for port := range container.NetworkSettings.Ports {
			proxyfile.Set(port.Port(), container.NetworkSettings.IPAddress)
		}
	}

	// write the proxyfile to the temp directory
	proxyfilePath := filepath.Join(dir, "proxy.sh")
	return ioutil.WriteFile(proxyfilePath, proxyfile.Bytes(), 0755)
}

// writeIdentifyFile is a helper function that
// will generate the id_rsa file in the builder's
// temp directory to be added to the Image.
func (b *Builder) writeIdentifyFile(dir string) error {
	keyfilePath := filepath.Join(dir, "id_rsa")
	return ioutil.WriteFile(keyfilePath, b.Key, 0700)
}

func getImage(service string) (*image, error) {
	tokens := strings.Split(service, " ")
	l := len(tokens)
	switch {
	// When service is a Drone official service
	case l == 1:
		image, ok := services[service]
		if !ok {
			return nil, fmt.Errorf("Error: Invalid or unknown service %s", service)
		}
		return image, nil
	// When service is a custom service
	case l == 2 || l == 3:
		ports := []string{}
		if l == 3 {
			ports = strings.Split(tokens[2], ",")
		}
		return &image{Name: tokens[0], Tag: tokens[1], Ports: ports}, nil
	default:
		return nil, fmt.Errorf("Error: Invalid service %s", service)
	}
}
