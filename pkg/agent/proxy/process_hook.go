package proxy

// OnProcess, when a build sets it, is told each process of the run the proxy
// sees, by its PID in the agent's PID namespace: the process of every
// connection it accepts, and the owner of a listener of the run's own tree
// that a connection is relayed to. A build that freezes the run's clock
// checks there, once per process, that each one reads it, and lists those
// that do not (design v14 §3.4, detection). It is called on the connection's
// own goroutine, so it must return at once and do its work elsewhere.
var OnProcess func(pid uint32)

// seeProcess tells OnProcess about pid; 0 is no process.
func seeProcess(pid uint32) {
	if f := OnProcess; f != nil && pid != 0 {
		f(pid)
	}
}
