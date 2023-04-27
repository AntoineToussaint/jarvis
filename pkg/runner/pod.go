package runner

import (
	"context"
	"fmt"
	"github.com/AntoineToussaint/kommence/pkg/configuration"
	"github.com/AntoineToussaint/kommence/pkg/output"
	"io"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var client *kubernetes.Clientset
var config *rest.Config

func LoadKubeClient(p string) {
	//home := homedir.HomeDir()
	//kubeconfig := path.Join(home, ".kube/config")
	var err error
	config, err = clientcmd.BuildConfigFromFlags("", p)
	if err != nil {
		panic(err.Error())
	}
	// creates the client
	client, err = kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
}

type Pod struct {
	config *configuration.Pod
	logger *output.Logger
}

func NewPod(logger *output.Logger, c *configuration.Pod) Runnable {
	return &Pod{
		logger: logger,
		config: c,
	}
}

func (p *Pod) ID() string {
	return fmt.Sprintf("⎈️ %v", p.config.ID)
}

func (p *Pod) Start(ctx context.Context, rec chan output.Message) error {
	// We need to get one pod
	p.logger.Debugf("looking for service %v in namespace %v\n", p.config.Service, p.config.Namespace)
	pods, err := client.CoreV1().Pods(p.config.Namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: fmt.Sprintf("app=%s", p.config.Service)})
	if err != nil {
		panic(err.Error())
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("no pod found in namespace %v", p.config.Namespace)
	}
	var pod v1.Pod = pods.Items[0]
	go func() {
		p.logger.Debugf("aggregating log for pod: %v\n", pod.Name)
		err = p.aggregateLog(ctx, pod, rec)
		if err != nil {
			p.logger.Errorf("can't aggregate log: %v", err)
		}
	}()
	go func() {
		err = p.forward(ctx, pod, rec)
		if err != nil {
			p.logger.Errorf("cannot forward: %v", err)
		}

	}()
	return nil
}

func (p *Pod) Stop(ctx context.Context, rec chan output.Message) error {
	p.logger.Debugf("stopping forwarding pod: %v\n", p.ID())
	return nil
}

func (p *Pod) forward(ctx context.Context, pod v1.Pod, rec chan output.Message) error {
	stream := genericclioptions.IOStreams{
		In:     os.Stdin,
		Out:    output.NewLineBreaker(rec, p.ID(), output.PodConnection),
		ErrOut: output.NewLineBreaker(rec, p.ID(), output.PodConnection),
	}
	//
	// stop control the port forwarding lifecycle. When it gets closed the
	// port forward will terminate
	stop := make(chan struct{}, 1)
	// ready communicate when the port forward is ready to get traffic
	ready := make(chan struct{})

	p.logger.Debugf("running port forward for pod %v %v:%v", pod.Name, p.config.LocalPort, p.config.PodPort)

	req := PortForwardAPodRequest{
		Pod:       pod,
		LocalPort: p.config.LocalPort,
		PodPort:   p.config.PodPort,
		Streams:   stream,
		Stop:      stop,
		Ready:     ready,
	}
	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return err
	}

	forwardPath := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", req.Pod.Namespace, req.Pod.Name)
	hostIP := strings.TrimLeft(config.Host, "https:/")

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, &url.URL{Scheme: "https", Path: forwardPath, Host: hostIP})

	fw, err := portforward.New(dialer, []string{fmt.Sprintf("%d:%d", req.LocalPort, req.PodPort)}, req.Stop, req.Ready, req.Streams.Out, req.Streams.ErrOut)
	if err != nil {
		return err
	}
	return fw.ForwardPorts()
}

func (p *Pod) aggregateLog(ctx context.Context, pod v1.Pod, rec chan output.Message) error {
	// Hack it for now
	// Log
	args := []string{"kubectl", "logs", pod.Name, "-n", p.config.Namespace, "-f"}
	// If a container is specified
	if container := p.config.Container; container != "" {
		args = append(args, container)
	}
	cmd := exec.Command(args[0], args[1:]...)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	err := cmd.Start()
	if err != nil {
		p.logger.Errorf("%v: cmd.Start() failed with '%s'\n", err)
	}

	go func() {
		_, _ = io.Copy(output.NewLineBreaker(rec, p.ID(), output.Log), stdout)
	}()
	_, _ = io.Copy(output.NewLineBreaker(rec, p.ID(), output.Error), stderr)
	return nil

}

// MatchPod based on name
func MatchPod(name string, pod string) bool {
	r, err := regexp.Compile(fmt.Sprintf(`%v-\w{7,10}-\w{5,7}`, name))
	if err != nil {
		return false
	}
	return r.Match([]byte(pod))
}

type PortForwardAPodRequest struct {
	// Pod is the selected pod for this port forwarding
	Pod v1.Pod
	// LocalPort is the local port that will be selected to expose the PodPort
	LocalPort int
	// PodPort is the target port for the pod
	PodPort int
	// Steams configures where to write or read input from
	Streams genericclioptions.IOStreams
	// Stop is the channel used to manage the port forward lifecycle
	Stop <-chan struct{}
	// Ready communicates when the tunnel is ready to receive traffic
	Ready chan struct{}
}
