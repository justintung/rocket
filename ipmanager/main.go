package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"runtime"

	"github.com/coreos/rocket/ipmanager/backend"
	"github.com/coreos/rocket/ipmanager/backend/disk"
)

var (
	containerID string
	networkName string
	store       backend.Store
)

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func main() {
	var err error
	flag.StringVar(&containerID, "c", "", "container id")
	flag.StringVar(&networkName, "n", "default", "network name")
	flag.Parse()
	conf, err := NewNetworkConfig(networkName)
	if err != nil {
		log.Fatal(err)
	}

	store, err = disk.New()
	if err != nil {
		log.Fatal(err)
	}

	allocator, err := NewIPAllocator(conf, store)
	if err != nil {
		log.Fatal(err)
	}
	allocation, err := allocator.Get(containerID)
	if err != nil {
		log.Fatal(err)
	}
	data, err := json.MarshalIndent(&allocation, "", "    ")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(data))
}
