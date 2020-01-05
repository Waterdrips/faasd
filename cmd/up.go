package cmd

import (
	"fmt"
	"github.com/pkg/errors"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexellis/faasd/pkg"
	"github.com/alexellis/k3sup/pkg/env"
	"github.com/spf13/cobra"
)

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Start faasd",
	RunE:  runUp,
}

const secretMountDir = "/run/secrets"

func runUp(cmd *cobra.Command, _ []string) error {

	wd, _ := cmd.Flags().GetString("work-dir")

	if len(wd) == 0 {
		wd = faasdwd
	}

	clientArch, clientOS := env.GetClientArch()

	if clientOS != "Linux" {
		return fmt.Errorf("you can only use faasd on Linux")
	}
	clientSuffix := ""
	switch clientArch {
	case "x86_64":
		clientSuffix = ""
		break
	case "armhf":
	case "armv7l":
		clientSuffix = "-armhf"
		break
	case "arm64":
	case "aarch64":
		clientSuffix = "-arm64"
	}

	if basicAuthErr := pkg.MakeBasicAuthFiles(path.Join(faasdwd, "secrets")); basicAuthErr != nil {
		return errors.Wrap(basicAuthErr, "cannot create basic-auth-* files")
	}

	services := makeServiceDefinitions(clientSuffix, wd)

	start := time.Now()
	supervisor, err := pkg.NewSupervisor("/run/containerd/containerd.sock")
	if err != nil {
		return err
	}

	log.Printf("Supervisor created in: %s\n", time.Since(start).String())

	start = time.Now()

	err = supervisor.Start(services, wd)

	if err != nil {
		return err
	}

	defer supervisor.Close()

	log.Printf("Supervisor init done in: %s\n", time.Since(start).String())

	shutdownTimeout := time.Second * 1
	timeout := time.Second * 60
	proxyDoneCh := make(chan bool)

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

		log.Printf("faasd: waiting for SIGTERM or SIGINT\n")
		<-sig

		log.Printf("Signal received.. shutting down server in %s\n", shutdownTimeout.String())
		err := supervisor.Remove(services)
		if err != nil {
			fmt.Println(err)
		}

		// Close proxy
		proxyDoneCh <- true
		time.AfterFunc(shutdownTimeout, func() {
			wg.Done()
		})
	}()

	gatewayURLChan := make(chan string, 1)
	proxyPort := 8080
	proxy := pkg.NewProxy(proxyPort, timeout)
	go proxy.Start(gatewayURLChan, proxyDoneCh)

	go func() {

		time.Sleep(3 * time.Second)

		fileData, fileErr := ioutil.ReadFile(path.Join(wd, "hosts"))
		if fileErr != nil {
			log.Println(fileErr)
			return
		}
		host := ""
		lines := strings.Split(string(fileData), "\n")
		for _, line := range lines {
			if strings.Index(line, "gateway") > -1 {
				host = line[:strings.Index(line, "\t")]
			}
		}
		log.Printf("[up] Sending %s to proxy\n", host)
		gatewayURLChan <- host + ":8080"
		close(gatewayURLChan)
	}()

	wg.Wait()
	return nil
}

func makeServiceDefinitions(archSuffix, workingDir string) []pkg.Service {
	secretDir := path.Join(workingDir, "secrets")
	wd, _ := os.Getwd()
	return []pkg.Service{
		pkg.Service{
			Name:  "basic-auth-plugin",
			Image: "docker.io/openfaas/basic-auth-plugin:0.18.10" + archSuffix,
			Env: []string{
				"port=8080",
				"secret_mount_path=" + secretMountDir,
				"user_filename=basic-auth-user",
				"pass_filename=basic-auth-password",
			},
			Mounts: []pkg.Mount{
				pkg.Mount{
					Src:  path.Join(secretDir, "basic-auth-password"),
					Dest: path.Join(secretMountDir, "basic-auth-password"),
				},
				pkg.Mount{
					Src:  path.Join(secretDir, "basic-auth-user"),
					Dest: path.Join(secretMountDir, "basic-auth-user"),
				},
			},
			Caps: []string{"CAP_NET_RAW"},
			Args: nil,
		},
		pkg.Service{
			Name:  "nats",
			Env:   []string{""},
			Image: "docker.io/library/nats-streaming:0.11.2",
			Caps:  []string{},
			Args:  []string{"/nats-streaming-server", "-m", "8222", "--store=memory", "--cluster_id=faas-cluster"},
		},
		pkg.Service{
			Name:  "prometheus",
			Env:   []string{},
			Image: "docker.io/prom/prometheus:v2.14.0",
			Mounts: []pkg.Mount{
				pkg.Mount{
					Src:  path.Join(wd, "prometheus.yml"),
					Dest: "/etc/prometheus/prometheus.yml",
				},
			},
			Caps: []string{"CAP_NET_RAW"},
		},
		pkg.Service{
			Name: "gateway",
			Env: []string{
				"basic_auth=true",
				"functions_provider_url=http://faas-containerd:8081/",
				"direct_functions=false",
				"read_timeout=60s",
				"write_timeout=60s",
				"upstream_timeout=65s",
				"faas_nats_address=nats",
				"faas_nats_port=4222",
				"auth_proxy_url=http://basic-auth-plugin:8080/validate",
				"auth_proxy_pass_body=false",
				"secret_mount_path=" + secretMountDir,
			},
			Image: "docker.io/openfaas/gateway:0.18.8" + archSuffix,
			Mounts: []pkg.Mount{
				pkg.Mount{
					Src:  path.Join(secretDir, "basic-auth-password"),
					Dest: path.Join(secretMountDir, "basic-auth-password"),
				},
				pkg.Mount{
					Src:  path.Join(secretDir, "basic-auth-user"),
					Dest: path.Join(secretMountDir, "basic-auth-user"),
				},
			},
			Caps: []string{"CAP_NET_RAW"},
		},
		pkg.Service{
			Name: "queue-worker",
			Env: []string{
				"faas_nats_address=nats",
				"faas_nats_port=4222",
				"gateway_invoke=true",
				"faas_gateway_address=gateway",
				"ack_wait=5m5s",
				"max_inflight=1",
				"write_debug=false",
				"basic_auth=true",
				"secret_mount_path=" + secretMountDir,
			},
			Image: "docker.io/openfaas/queue-worker:0.9.0",
			Mounts: []pkg.Mount{
				pkg.Mount{
					Src:  path.Join(secretDir, "basic-auth-password"),
					Dest: path.Join(secretMountDir, "basic-auth-password"),
				},
				pkg.Mount{
					Src:  path.Join(secretDir, "basic-auth-user"),
					Dest: path.Join(secretMountDir, "basic-auth-user"),
				},
			},
			Caps: []string{"CAP_NET_RAW"},
		},
	}
}
