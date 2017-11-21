package main

import (
	"bufio"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Shopify/sarama"
	"github.com/satori/go.uuid"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/mem"
	hnet "github.com/shirou/gopsutil/net"
)

// Object represents something that can be sent to the backend. It must have a
// topic and implement a brand() method that fills UUID and checksum fields.
type Object interface {
	topic() string
	brand()
}

func checksum(path string) string {
	f, err := os.Open(path)
	if err != nil {
		log.Panic(err)
	}
	defer f.Close()

	h := sha512.New512_224()
	if _, err := io.Copy(h, f); err != nil {
		log.Panic(err)
	}

	hash := h.Sum(nil)
	sum := fmt.Sprintf("%x", hash)
	//log.Println("checksum():", path, sum)
	return sum
}

// System contains data pertaining to overall system metrics
type System struct {
	CPUPercent float64 `json:"cpu_percent"`
	MemPercent float64 `json:"mem_percent"`
	Inbound    uint64  `json:"inbound_traffic"`
	Outbound   uint64  `json:"outbound_traffic"`
}

// Event contains data pertaining to the termination of a child process.
type Event struct {
	CheckSum      string    `json:"checksum"`
	UUID          string    `json:"uuid"`
	Time          time.Time `json:"timestamp"`
	Status        int       `json:"exit_status"`
	Signal        string    `json:"signal,omitempty"`
	SystemMetrics System    `json:"system_metrics"`
}

func (e Event) topic() string {
	return envar["EVENT_TOPIC"]
}

func (e *Event) brand() {
	e.UUID = uuid.NewV4().String()
	e.CheckSum = cksum
}

func event(state *os.ProcessState) *Event {
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		log.Print("expected type syscall.WaitStatus; non-POSIX system?")
		return nil
	}

	var (
		inbound    uint64
		outbound   uint64
		cpuPercent float64
		memPercent float64
	)

	/* System-wide cpu usage since the start of the child process */
	if tempCPU, err := cpu.Percent(0, false); err == nil {
		cpuPercent = tempCPU[0]
	}

	/*System-wide current virtual memory (ram) consumption
	percentage at the time of child process termination */
	if tempMem, err := mem.VirtualMemory(); err == nil {
		memPercent = tempMem.UsedPercent
	}

	/* Total network I/O bytes recieved and sent from the system
	since the start of the system */
	if tempNet, err := hnet.IOCounters(false); err == nil {
		inbound = tempNet[0].BytesRecv
		outbound = tempNet[0].BytesSent
	}

	s := System{
		CPUPercent: cpuPercent,
		MemPercent: memPercent,
		Inbound:    inbound,
		Outbound:   outbound,
	}

	return &Event{
		Time:   time.Now(),
		Status: ws.ExitStatus(),
		Signal: func() string {
			if ws.Signaled() {
				return ws.Signal().String()
			}
			return ""
		}(),
		SystemMetrics: s,
	}
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func usage() {
	log.Fatalf("usage: %v command [args ...]\n", os.Args[0])
}

var inboundPrev, outboundPrev uint64

func run(obj chan Object, cmd *exec.Cmd) {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	log.Print("starting child")
	err := cmd.Start()
	if err != nil {
		panic(err)
	}

	cpu.Percent(0, false)
	done := make(chan struct{})
	sig := make(chan os.Signal)
	signal.Notify(sig, syscall.SIGINT)

	go func() {
		cmd.Wait()
		obj <- event(cmd.ProcessState)
		done <- struct{}{}
	}()

	for {
		select {
		case s := <-sig:
			log.Print("relaying signal: ", s)
			cmd.Process.Signal(s)
		case <-done:
			log.Print("child exited")
			return
		}
	}
}

// Profile represents arbitrary JSON data from the instrument that can be sent
// to the backend.
type Profile struct {
	CheckSum string      `json:"checksum,omitempty"`
	UUID     string      `json:"uuid,omitempty"`
	Profile  interface{} `json:"profile"`
}

func (p Profile) topic() string {
	return envar["PROF_TOPIC"]
}

func (p *Profile) brand() {
	p.UUID = uuid.NewV4().String()
	p.CheckSum = cksum
}

func logs(logger io.Writer) (func(), error) {
	l, err := net.Listen("unixpacket", "log-"+strconv.Itoa(os.Getpid()))
	if err != nil {
		return func() {}, err
	}
	log.Print("logs socket opened")

	done := make(chan error)
	go func() {
		c, err := l.Accept()
		if err != nil {
			done <- err
		}
		log.Print("logs connection accepted")

		t := io.TeeReader(c, logger)
		_, err = ioutil.ReadAll(t)
		done <- err
	}()

	return func() {
		if err := <-done; err != nil {
			log.Print(err)
		}
		log.Print("closing logs socket")
		l.Close()
	}, nil
}

func relay(obj chan Object) (func(), error) {
	s, err := net.Listen("unix", "data-"+strconv.Itoa(os.Getpid()))
	if err != nil {
		return func() {}, err
	}
	log.Print("data socket opened")

	done := make(chan error)

	go func() {
		c, err := s.Accept()
		if err != nil {
			done <- err
		}
		log.Print("data connection accepted")
		line := bufio.NewScanner(c)

		// quits on EOF
		for line.Scan() {
			var p Profile
			err := json.Unmarshal(line.Bytes(), &p.Profile)
			if err != nil {
				done <- err
				return
			}
			obj <- &p
		}
		log.Print("data socket EOF")
		done <- nil
	}()

	return func() {
		// wait for socket relay to finish
		if err := <-done; err != nil {
			log.Print(err)
		}
		log.Print("closing data socket")
		s.Close()
	}, nil
}

func decode(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	check(err)
	return b
}

func connect() (sarama.SyncProducer, error) {
	ca := decode(envar["CA"])
	cert := decode(envar["CERT"])
	key := decode(envar["PRIVATE_KEY"])

	certpool := x509.NewCertPool()
	certpool.AppendCertsFromPEM(ca)
	c, err := tls.X509KeyPair(cert, key)
	check(err)

	tc := tls.Config{
		RootCAs:            certpool,
		ClientAuth:         tls.NoClientCert,
		ClientCAs:          nil,
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{c},
	}

	config := sarama.NewConfig()
	config.Producer.Return.Successes = true
	config.Net.TLS.Enable = true
	config.Net.TLS.Config = &tc
	config.ClientID = "ProfileTest"

	brokers := strings.Split(envar["BROKERS"], ",")
	return sarama.NewSyncProducer(brokers, config)
}

func produce(obj chan Object) (func(), error) {
	// Create a Kafka producer with the desired config
	p, err := connect()
	if err != nil {
		// bad config or closed client
		return func() {}, err
	}
	log.Println("kafka producer connected")

	done := make(chan error)
	go func() {
		// receive Kafka-bound objects from clients
		for o := range obj {
			o.brand()
			b, err := json.Marshal(o)
			if err != nil {
				done <- err
				return
			}
			log.Printf("producer got %v bytes: %v", len(b), string(b))
			//log.Printf("producer got %v bytes", len(b))
			_, _, err = p.SendMessage(&sarama.ProducerMessage{
				Topic: o.topic(),
				Value: sarama.ByteEncoder(b),
			})
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	return func() {
		// wait for kafka producer to finish
		if err := <-done; err != nil {
			log.Print(err)
		}
		log.Print("closing kafka producer")
		p.Close()
	}, nil
}

var cksum string

func valid(sum string) bool {
	ep := envar["BASE_URL"] + "/check_releases/" + sum
	//log.Println("wrapper: release check url:", ep)
	resp, err := http.Get(ep)
	if err != nil {
		log.Panic(err)
	}
	//log.Println("wrapper: valid: response status:", resp.Status)

	switch resp.StatusCode {
	case 200:
		return true
	case 404:
		return false
	default:
		log.Panic("wrapper: valid: got unexpected status ", resp.Status)
	}
	return false
}

var envar map[string]string

func env() {
	envar = make(map[string]string)
	keys := []string{
		"BASE_URL",
		"BROKERS",
		"PROF_TOPIC",
		"EVENT_TOPIC",
		"CA",
		"CERT",
		"PRIVATE_KEY",
	}

	prefix := "AUKLET_"
	ok := true
	for _, k := range keys {
		v := os.Getenv(prefix + k)
		if v == "" {
			ok = false
			log.Printf("empty envar %v\n", prefix+k)
		} else {
			envar[k] = v
		}
	}
	if !ok {
		log.Fatal("incomplete configuration")
	}
}

func main() {
	logger := os.Stdout
	log.SetOutput(logger)

	env()

	args := os.Args
	if len(args) < 2 {
		usage()
	}
	cmd := exec.Command(args[1], args[2:]...)

	cksum = checksum(cmd.Path)
	if !valid(cksum) {
		//log.Fatal("invalid checksum: ", cksum)
		log.Print("invalid checksum: ", cksum)
	}

	obj := make(chan Object)

	wprod, err := produce(obj)
	check(err)
	defer wprod()

	wrelay, err := relay(obj)
	check(err)
	defer wrelay()

	lc, err := logs(logger)
	check(err)
	defer lc()

	run(obj, cmd)
	close(obj)
}
