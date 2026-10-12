package containerkey

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Default is the agent's keyer: the proxy names each connection with it, the
// scope routes open and close its test scopes, and the outgoing stream stamps
// recorded mocks with it.
var Default = NewKeyer(newDockerResolver())

// resolver finds the container behind a destination; ok is false for any
// destination that is not one. err says Docker could not be asked.
type resolver interface {
	Resolve(ctx context.Context, dst netip.AddrPort) (c Container, ok bool, err error)
	// Endpoint names the Docker endpoint asked, for messages.
	Endpoint() string
}

// Keyer numbers container destinations and remembers each connection's key.
//
// A key's number counts the image's containers by their first connection
// within the scope (design §3.1; one test worker per run, so the scope is the
// open test, or the run outside tests). A test scope starts from the numbers
// the run gave before the test began, so a container the suite set up keeps
// its number inside every test, and one the test starts itself is numbered
// after it: the same in a run of that one test as in a run of all of them.
// A connection keeps the key it was given when it was accepted, for as long
// as it lives.
//
// Numbers follow connection order and nothing else, so a run that connects
// in the same order gets the same keys on any machine; a run that connects in
// another order gets other numbers, so its calls miss.
type Keyer struct {
	mu sync.Mutex
	// res is nil where keploy cannot ask Docker at all (Disable).
	res resolver
	// run numbers the run's containers outside tests; test, while a test is
	// open, numbers that test's.
	run, test *numbering
	// conns is each container connection's key, by the connection ID its
	// mocks carry, for Stamp. Cleared with the session.
	conns map[string]string
	// warned: this session already said Docker could not be asked.
	warned bool
}

// NewKeyer is a keyer that asks res which destinations are containers.
func NewKeyer(res resolver) *Keyer {
	return &Keyer{res: res, run: newNumbering()}
}

// Disable stops k asking Docker: every destination is named by its address,
// as without the switch. keploy's agent container has no Docker socket, so a
// docker-mode agent disables its keyer at start, and its compose services
// keep being matched by the names the app calls them by.
func (k *Keyer) Disable() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.res = nil
}

// Reset starts a new session (a record or a replay of one test set): every
// number and every connection's key goes.
func (k *Keyer) Reset() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.run, k.test, k.conns, k.warned = newNumbering(), nil, nil, false
}

// BeginTest opens a test's scope; a scope already open is replaced, since a
// run has one open test at a time.
func (k *Keyer) BeginTest() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.test = k.run.clone()
}

// EndTest closes the open test's scope: connections from here on are the
// run's again.
func (k *Keyer) EndTest() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.test = nil
}

// Name resolves the destination of the connection connID, the address the
// proxy dials (ip:port), and returns its container and key when it is a
// container. The key is fixed here, in the scope open now, for the
// connection's life.
//
// When Docker cannot be asked, the destination is named by its address, as
// without the switch, and the session says so once: its container
// destinations are then compared by address, so one on a random port misses
// in replay instead of matching another container.
func (k *Keyer) Name(ctx context.Context, logger *zap.Logger, connID, dst string) (Container, string, bool) {
	k.mu.Lock()
	res := k.res
	k.mu.Unlock()
	ap, err := parseAddrPort(dst)
	if err != nil || res == nil {
		return Container{}, "", false
	}
	c, ok, err := res.Resolve(ctx, ap)
	k.mu.Lock()
	defer k.mu.Unlock()
	if err != nil {
		if !k.warned && ctx.Err() == nil && logger != nil {
			k.warned = true
			logger.Warn("keploy could not ask Docker which container a connection goes to, so this run names container destinations by their address: a container on a random port, such as one Testcontainers starts, will not match its recording",
				zap.String("dockerEndpoint", res.Endpoint()), zap.String("destination", dst), zap.Error(err),
				zap.String("next_step", "let keploy's agent reach the Docker daemon the tests use: it asks the endpoint DOCKER_HOST names when keploy starts, else the default socket"))
		}
		return Container{}, "", false
	}
	if !ok {
		return Container{}, "", false
	}
	s := k.run
	if k.test != nil {
		s = k.test
	}
	key := Key{Image: c.Image, Port: c.Port, N: s.number(c)}.String()
	if connID != "" {
		if k.conns == nil {
			k.conns = map[string]string{}
		}
		k.conns[connID] = key
	}
	return c, key, true
}

// Stamp writes the key of the connection a recorded mock came from into its
// metadata (MetadataKey). A mock of any other connection is left as it is.
func (k *Keyer) Stamp(m *models.Mock) {
	if m == nil {
		return
	}
	id := m.ConnectionID
	if id == "" {
		id = m.Spec.Metadata["connID"]
	}
	if id == "" {
		return
	}
	k.mu.Lock()
	key := k.conns[id]
	k.mu.Unlock()
	if key == "" {
		return
	}
	if m.Spec.Metadata == nil {
		m.Spec.Metadata = map[string]string{}
	}
	m.Spec.Metadata[MetadataKey] = key
}

// numbering is one scope's numbers: per image, each container's number, and
// how many the image has.
type numbering struct {
	byImage map[string]*imageNumbers
}

type imageNumbers struct {
	of    map[string]int // container ID -> its number
	count int
}

func newNumbering() *numbering { return &numbering{byImage: map[string]*imageNumbers{}} }

// number is c's number in this scope, giving it the image's next one on its
// first connection here.
func (s *numbering) number(c Container) int {
	im := s.byImage[c.Image]
	if im == nil {
		im = &imageNumbers{of: map[string]int{}}
		s.byImage[c.Image] = im
	}
	if n, ok := im.of[c.ID]; ok {
		return n
	}
	im.count++
	im.of[c.ID] = im.count
	return im.count
}

func (s *numbering) clone() *numbering {
	out := newNumbering()
	for image, im := range s.byImage {
		of := make(map[string]int, len(im.of))
		for id, n := range im.of {
			of[id] = n
		}
		out.byImage[image] = &imageNumbers{of: of, count: im.count}
	}
	return out
}

// parseAddrPort reads ip:port, IPv6 in brackets, an IPv4-mapped IPv6 address
// read as IPv4.
func parseAddrPort(s string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return netip.AddrPort{}, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.AddrPort{}, err
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(ip.Unmap(), uint16(p)), nil
}
