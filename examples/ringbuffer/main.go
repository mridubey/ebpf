//go:build linux
// +build linux

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/cilium/ebpf/internal"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/perf"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"
)

// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS -target 386 -type event -type tcpevent -type piddata -type latdata bpf ringbuffer.c -- -I../headers

func main() {
	// Name of the kernel function to trace.
	fn1 := "__sys_recvfrom"
	fn2 := "__sys_sendto"
	fn3 := "tcp_connect"
	fn4 := "tcp_rcv_state_process"

	// Subscribe to signals for terminating the program.
	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, os.Interrupt, syscall.SIGTERM)

	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatal(err)
	}

	// Load pre-compiled programs and maps into the kernel.
	objs := bpfObjects{}
	if err := loadBpfObjects(&objs, nil); err != nil {
		log.Fatalf("loading objects: %v", err)
	}
	defer objs.Close()

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program. Each time the kernel function enters, the program
	// will emit an event containing pid and command of the execved task.
	kp1, err := link.Kprobe(fn1, objs.KprobeRecvfrom, nil)
	if err != nil {
		log.Fatalf("opening kprobe: %s", err)
	}
	defer kp1.Close()

	kp2, err := link.Kprobe(fn2, objs.KprobeSendto, nil)
	if err != nil {
		log.Fatalf("opening kprobe: %s", err)
	}
	defer kp2.Close()

	kp3, err := link.Kprobe(fn3, objs.TcpConnect, nil)
	if err != nil {
		log.Fatalf("opening kprobe: %s", err)
	}
	defer kp3.Close()

	kp4, err := link.Kprobe(fn4, objs.TcpRcvStateProcess, nil)
	if err != nil {
		log.Fatalf("opening kprobe: %s", err)
	}
	defer kp4.Close()

	link, err := link.AttachTracing(link.TracingOptions{
		Program: objs.bpfPrograms.TcpClose,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer link.Close()

	// Open a ringbuf reader from userspace RINGBUF map described in the
	// eBPF C program.
	rd1, err := ringbuf.NewReader(objs.Events)
	if err != nil {
		log.Fatalf("opening ringbuf reader: %s", err)
	}
	defer rd1.Close()

	rd2, err := ringbuf.NewReader(objs.bpfMaps.Tcpevents)
	if err != nil {
		log.Fatalf("opening ringbuf reader: %s", err)
	}
	defer rd2.Close()

	// Open a perf event reader from userspace on the PERF_EVENT_ARRAY map
	// described in the eBPF C program.
	rd3, err := perf.NewReader(objs.Latdatas, os.Getpagesize())
	if err != nil {
		log.Fatalf("creating perf event reader: %s", err)
	}
	defer rd3.Close()

	log.Printf("Listening for events..")

	// go func() {
	//  	// Wait for a signal and close the perf reader,
	// 	// which will interrupt rd.Read() and make the program exit.
	// 	<-stopper

	// 	log.Println("Received signal, exiting program..")
	// 	if err := rd1.Close(); err != nil {
	// 		log.Fatalf("closing ringbuf reader 1: %s", err)
	// 	}
	// 	if err := rd2.Close(); err != nil {
	// 		log.Fatalf("closing ringbuf reader 2: %s", err)
	// 	}
	//  if err := rd3.Close(); err != nil {
	// 		log.Fatalf("closing perf event reader: %s", err)
	// 	}
	// }()

	go readLoopSipMessages(rd1)
	go readLoopTcpClose(rd2)
	go readLoopTcpLatency(rd3)

	<-stopper
}

func readLoopTcpLatency(rd *perf.Reader) {
	// bpfEvent is generated by bpf2go.
	var event bpfLatdata
	for {
		record, err := rd.Read()
		if err != nil {
			if errors.Is(err, perf.ErrClosed) {
				return
			}
			log.Printf("reading from perf event reader: %s", err)
			continue
		}

		if record.LostSamples != 0 {
			log.Printf("perf event ring buffer full, dropped %d samples", record.LostSamples)
			continue
		}

		// Parse the perf event entry into a bpfEvent structure.
		if err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event); err != nil {
			log.Printf("parsing perf event: %s", err)
			continue
		}

		if event.Comm[0] == 115 && event.Comm[1] == 105 && event.Comm[2] == 112 && event.Comm[3] == 112 {
			log.Printf("Latency: %.2f\tcomm: %s", float64(event.DeltaUs)/1000.0, unix.ByteSliceToString(event.Comm[:]))
		}
	}
}

func readLoopSipMessages(rd *ringbuf.Reader) {
	// bpfEvent is generated by bpf2go.
	var event bpfEvent
	for {
		record, err := rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				log.Println("Received signal, exiting..")
				return
			}
			log.Printf("reading from reader: %s", err)
			continue
		}

		// Parse the ringbuf event entry into a bpfEvent structure.
		if err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event); err != nil {
			log.Printf("parsing ringbuf event: %s", err)
			continue
		}

		if event.Comm[0] == 115 && event.Comm[1] == 105 && event.Comm[2] == 112 && event.Comm[3] == 112 {
			log.Printf("pid: %d\tfd: %d\tlen: %d\tcomm: %s\n%s \n\n", event.Pid, event.Fd, event.Len, unix.ByteSliceToString(event.Comm[:]), unix.ByteSliceToString(event.Msg[:]))
		}
	}
}

func readLoopTcpClose(rd *ringbuf.Reader) {
	// bpfEvent is generated by bpf2go.
	var tcpevent bpfTcpevent
	for {
		record, err := rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				log.Println("received signal, exiting..")
				return
			}
			log.Printf("reading from reader: %s", err)
			continue
		}

		// Parse the ringbuf event entry into a bpfEvent structure.
		if err := binary.Read(bytes.NewBuffer(record.RawSample), internal.NativeEndian, &tcpevent); err != nil {
			log.Printf("parsing ringbuf event: %s", err)
			continue
		}

		if tcpevent.Comm[0] == 115 && tcpevent.Comm[1] == 105 && tcpevent.Comm[2] == 112 && tcpevent.Comm[3] == 112 {
			log.Printf("%-15s %-6d -> %-15s %-6d %.2f %-6s",
				intToIP(tcpevent.Saddr),
				tcpevent.Sport,
				intToIP(tcpevent.Daddr),
				tcpevent.Dport,
				float64(tcpevent.Srtt)/1000.0,
				unix.ByteSliceToString(tcpevent.Comm[:]),
			)
		}
	}
}

// intToIP converts IPv4 number to net.IP
func intToIP(ipNum uint32) net.IP {
	ip := make(net.IP, 4)
	internal.NativeEndian.PutUint32(ip, ipNum)
	return ip
}
