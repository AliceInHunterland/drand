package lib

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	json "github.com/nikkolasg/hexjson"

	"github.com/drand/drand/chain"
	"github.com/drand/drand/common"
	"github.com/drand/drand/crypto"
	"github.com/drand/drand/demo/cfg"
	"github.com/drand/drand/demo/node"
	"github.com/drand/drand/key"
	"github.com/drand/drand/protobuf/drand"
)

// 1s after dkg finishes, (new or reshared) beacon starts
var beaconOffset = 2

// how much should we wait before checking if the randomness is present. This is
// mostly due to the fact we run on localhost on cheap machine with CI so we
// need some delays to make sure *all* nodes that we check have gathered the
// randomness.
var afterPeriodWait = 5 * time.Second

// Orchestrator controls a set of nodes
type Orchestrator struct {
	n                 int
	thr               int
	newThr            int
	beaconID          string
	period            string
	scheme            *crypto.Scheme
	periodD           time.Duration
	basePath          string
	groupPath         string
	newGroupPath      string
	certFolder        string
	nodes             []node.Node
	paths             []string
	newNodes          []node.Node
	newPaths          []string
	genesis           int64
	transition        int64
	group             *key.Group
	newGroup          *key.Group
	resharePaths      []string
	reshareIndex      []int
	reshareNodes      []node.Node
	tls               bool
	withCurl          bool
	isBinaryCandidate bool
	binary            string
	dbEngineType      chain.StorageType
	pgDSN             func() string
	memDBSize         int
}

func NewOrchestrator(c cfg.Config) *Orchestrator {
	c.BasePath = path.Join(os.TempDir(), "drand-full")
	// cleanup the basePath before doing anything
	_ = os.RemoveAll(c.BasePath)

	fmt.Printf("[+] Simulation global folder: %s\n", c.BasePath)
	checkErr(os.MkdirAll(c.BasePath, 0o740))
	c.CertFolder = path.Join(c.BasePath, "certs")
	c.BeaconID = common.GetCanonicalBeaconID(c.BeaconID)

	checkErr(os.MkdirAll(c.CertFolder, 0o740))
	nodes, paths := createNodes(c)

	periodD, err := time.ParseDuration(c.Period)
	checkErr(err)
	e := &Orchestrator{
		n:                 c.N,
		thr:               c.Thr,
		scheme:            c.Scheme,
		basePath:          c.BasePath,
		groupPath:         path.Join(c.BasePath, "group.toml"),
		newGroupPath:      path.Join(c.BasePath, "group2.toml"),
		period:            c.Period,
		periodD:           periodD,
		nodes:             nodes,
		paths:             paths,
		certFolder:        c.CertFolder,
		tls:               c.WithTLS,
		withCurl:          c.WithCurl,
		binary:            c.Binary,
		isBinaryCandidate: c.IsCandidate,
		beaconID:          common.GetCanonicalBeaconID(c.BeaconID),
		dbEngineType:      c.DBEngineType,
		pgDSN:             c.PgDSN,
		memDBSize:         c.MemDBSize,
	}
	return e
}

func (e *Orchestrator) StartCurrentNodes(toExclude ...int) {
	filtered := filterNodes(e.nodes, toExclude...)
	e.startNodes(filtered)
}

func (e *Orchestrator) StartNewNodes() {
	e.startNodes(e.newNodes)
}

func (e *Orchestrator) startNodes(nodes []node.Node) {
	fmt.Printf("[+] Starting all nodes\n")
	for _, n := range nodes {
		fmt.Printf("\t- Starting node %s\n", n.PrivateAddr())
		n.Start(e.certFolder, e.dbEngineType, e.pgDSN, e.memDBSize)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// ping them all
	for {
		select {
		case <-ticker.C:
			var foundAll = true
			for _, n := range nodes {
				if !n.Ping() {
					foundAll = false
					break
				}
			}

			if !foundAll {
				fmt.Println("[-] can not ping them all. Sleeping 2s...")
				break
			}
			return
		case <-ctx.Done():
			fmt.Println("[-] can not ping all nodes in 30 seconds. Shutting down.")
			panic("failed to ping nodes in 30 seconds")
		}
	}
}

func (e *Orchestrator) RunDKG(timeout time.Duration) {
	fmt.Println("[+] Running DKG for all nodes")
	time.Sleep(100 * time.Millisecond)
	startTime := time.Now() // Start timing the DKG process

	leader := e.nodes[0]
	var wg sync.WaitGroup
	wg.Add(len(e.nodes))
	panicCh := make(chan interface{}, 1)
	go func() {
		defer func() {
			if err := recover(); err != nil {
				panicCh <- err
			}
			wg.Done()
		}()
		fmt.Printf("\t- Running DKG for leader node %s\n", leader.PrivateAddr())
		leader.RunDKG(e.n, e.thr, timeout, true, "", beaconOffset)
	}()
	time.Sleep(200 * time.Millisecond)
	for _, n := range e.nodes[1:] {
		n := n
		fmt.Printf("\t- Running DKG for node %s\n", n.PrivateAddr())
		go func(n node.Node) {
			n.RunDKG(e.n, e.thr, timeout, false, leader.PrivateAddr(), beaconOffset)
			fmt.Println("\t FINISHED DKG")
			if err := recover(); err != nil {
				panicCh <- err
			}
			wg.Done()
		}(n)
	}
	wg.Wait()
	select {
	case p := <-panicCh:
		panic(p)
	default:
	}

	duration := time.Since(startTime) // Calculate the duration of the DKG process
	fmt.Println(duration)
	fmt.Println("[+] Nodes finished running DKG. Checking keys...")
	// we pass the current group path
	startTime = time.Now()
	g := e.checkDKGNodes(e.nodes, e.groupPath)
	KeysDuration := time.Since(startTime)
	// overwrite group to group path
	e.group = g
	e.genesis = g.GenesisTime
	checkErr(key.Save(e.groupPath, e.group, false))
	fmt.Println("\t- Overwrite group with distributed key to ", e.groupPath)
	logToFile(len(e.nodes), duration, KeysDuration)
}

func logToFile(nodeCount int, duration time.Duration, KeysDuration time.Duration) {
	file, err := os.OpenFile("./test.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Error opening file: %v\n", err)
		return
	}
	defer file.Close()

	_, err = fmt.Fprintf(file, "%d,%v,%v\n", nodeCount, duration, KeysDuration)
	if err != nil {
		fmt.Printf("Error writing to file: %v\n", err)
	}
}

func (e *Orchestrator) checkDKGNodes(nodes []node.Node, groupPath string) *key.Group {
	for {
		fmt.Println("[+] Checking if chain info is present on all nodes...")
		var allFound = true
		for _, n := range nodes {
			if !n.ChainInfo(groupPath) {
				allFound = false
				break
			}
		}
		if !allFound {
			fmt.Println("[+] Chain info not present on all nodes. Sleeping 3s...")
			time.Sleep(3 * time.Second)
		} else {
			fmt.Println("[+] Chain info are present on all nodes. DKG finished.")
			break
		}
	}

	var g *key.Group
	var lastNode string
	fmt.Println("[+] Checking all created group file with collective key")
	for _, n := range nodes {
		group := n.GetGroup()
		if g == nil {
			g = group
			lastNode = n.PrivateAddr()
			continue
		}
		if !g.PublicKey.Equal(group.PublicKey) {
			panic(fmt.Errorf("[-] Node %s has different cokey than %s", n.PrivateAddr(), lastNode))
		}
	}
	return g
}

func (e *Orchestrator) WaitGenesis() {
	to := time.Until(time.Unix(e.genesis, 0))
	fmt.Printf("[+] Sleeping %d until genesis happens\n", int(to.Seconds()))
	time.Sleep(to)
	relax := 3 * time.Second
	fmt.Printf("[+] Sleeping %s after genesis - leaving some time for rounds \n", relax)
	time.Sleep(relax)
}

func (e *Orchestrator) WaitTransition() {
	to := time.Until(time.Unix(e.transition, 0))
	currentRound := chain.CurrentRound(e.transition, e.periodD, e.genesis)

	fmt.Printf("[+] Sleeping %s until transition happens (transition time: %d) currentRound: %d\n", to, e.transition, currentRound)
	time.Sleep(to)
	fmt.Printf("[+] Sleeping %s after transition - leaving some time for nodes\n", afterPeriodWait)
	time.Sleep(afterPeriodWait)
}

func (e *Orchestrator) Wait(t time.Duration) {
	fmt.Printf("[+] Sleep %ss to leave some time to sync & start again\n", t)
	time.Sleep(t)
}

func (e *Orchestrator) WaitPeriod() {
	nRound, nTime := chain.NextRound(time.Now().Unix(), e.periodD, e.genesis)
	until := time.Until(time.Unix(nTime, 0).Add(afterPeriodWait))

	fmt.Printf("[+] Sleeping %ds to reach round %d + 3s\n", int(until.Seconds()), nRound)
	time.Sleep(until)
}

func (e *Orchestrator) CheckCurrentBeacon(exclude ...int) {
	filtered := filterNodes(e.nodes, exclude...)
	e.checkBeaconNodes(filtered, e.groupPath, e.withCurl)
}

func (e *Orchestrator) CheckNewBeacon(exclude ...int) {
	filtered := filterNodes(e.reshareNodes, exclude...)
	e.checkBeaconNodes(filtered, e.newGroupPath, e.withCurl)
}

func filterNodes(list []node.Node, exclude ...int) []node.Node {
	var filtered []node.Node
	for _, n := range list {
		var isExcluded = false
		for _, i := range exclude {
			if i == n.Index() {
				isExcluded = true
				break
			}
		}
		if !isExcluded {
			filtered = append(filtered, n)
		}
	}
	rand.Shuffle(len(filtered), func(i, j int) {
		filtered[i], filtered[j] = filtered[j], filtered[i]
	})
	return filtered
}

func (e *Orchestrator) checkBeaconNodes(nodes []node.Node, group string, tryCurl bool) {
	nRound, _ := chain.NextRound(time.Now().Unix(), e.periodD, e.genesis)
	currRound := nRound - 1
	fmt.Printf("[+] Checking randomness beacon for round %d via CLI\n", currRound)
	var pubRand *drand.PublicRandResponse
	var lastIndex int
	for _, n := range nodes {
		const maxTrials = 3
		for i := 0; i < maxTrials; i++ {
			randResp, cmd := n.GetBeacon(group, currRound)
			if pubRand == nil {
				pubRand = randResp
				lastIndex = n.Index()
				fmt.Printf("\t - Example command is: \"%s\"\n", cmd)
				break
			}

			// we first check both are at the same round
			if randResp.GetRound() != pubRand.GetRound() {
				fmt.Println("[-] Mismatch between last index", lastIndex, " vs current index ", n.Index(), " - trying again in some time...")
				time.Sleep(100 * time.Millisecond)
				// we try again
				continue
			}
			// then we check if the signatures match
			if !bytes.Equal(randResp.GetSignature(), pubRand.GetSignature()) {
				panic("[-] Inconsistent beacon signature between nodes")
			}
			// everything is good
			break
		}
	}
	fmt.Println("[+] Checking randomness via HTTP API using curl")
	var printed bool
	for _, n := range nodes {
		args := []string{"-k", "-s"}
		http := "http"
		if e.tls {
			tmp, _ := os.CreateTemp("", "cert")
			tmpName := tmp.Name() // Extract the name into a separate variable and then use it in the defer call
			defer func() {
				_ = os.Remove(tmpName)
			}()
			_ = tmp.Close()
			n.WriteCertificate(tmpName)
			args = append(args, pair("--cacert", tmpName)...)
			http = http + "s"
		}
		args = append(args, pair("-H", "Context-type: application/json")...)
		url := http + "://" + n.PublicAddr() + "/public/"
		// add the round to make sure we don't ask for a later block if we're
		// behind
		url += strconv.Itoa(int(currRound))
		args = append(args, url)

		const maxCurlRetries = 10
		for i := 0; i < maxCurlRetries; i++ {
			cmd := exec.Command("curl", args...)
			if !printed {
				fmt.Printf("\t- Example command: \"%s\"\n", strings.Join(cmd.Args, " "))
				printed = true
			}
			if tryCurl {
				// curl returns weird error code
				out, _ := cmd.CombinedOutput()
				if len(out) == 0 {
					fmt.Println("received empty response from curl. Retrying ...")
					time.Sleep(afterPeriodWait)
					continue
				}

				out = append(out, []byte("\n")...)
				var r = new(drand.PublicRandResponse)
				checkErr(json.Unmarshal(out, r), string(out))
				if r.GetRound() != pubRand.GetRound() {
					panic("[-] Inconsistent round from curl vs CLI")
				} else if !bytes.Equal(r.GetSignature(), pubRand.GetSignature()) {
					fmt.Printf("curl output: %s\n", out)
					fmt.Printf("curl output rand: %x\n", r.GetSignature())
					fmt.Printf("cli output: %s\n", pubRand)
					fmt.Printf("cli output rand: %x\n", pubRand.GetSignature())
					panic("[-] Inconsistent signature from curl vs CLI")
				}
			} else {
				fmt.Printf("\t[-] Issue with curl command at the moment\n")
			}
			break
		}
	}
	out, err := json.MarshalIndent(pubRand, "", "    ")
	checkErr(err)
	fmt.Printf("%s\n", out)
}

func (e *Orchestrator) SetupNewNodes(n int) {
	fmt.Printf("[+] Setting up %d new nodes for resharing\n", n)
	c := cfg.Config{
		N:            n,
		Offset:       len(e.nodes) + 1,
		Period:       e.period,
		BasePath:     e.basePath,
		CertFolder:   e.certFolder,
		WithTLS:      e.tls,
		Binary:       e.binary,
		Scheme:       e.scheme,
		BeaconID:     e.beaconID,
		IsCandidate:  e.isBinaryCandidate,
		DBEngineType: e.dbEngineType,
		PgDSN:        e.pgDSN,
		MemDBSize:    e.memDBSize,
	}
	//  offset int, period, basePath, certFolder string, tls bool, binary string, sch scheme.Scheme, beaconID string, isCandidate bool
	e.newNodes, e.newPaths = createNodes(c)
}

// UpdateBinary will set the 'binary' to use for the node at 'idx'
func (e *Orchestrator) UpdateBinary(binary string, idx uint, isCandidate bool) {
	n := e.nodes[idx]
	if spn, ok := n.(*node.NodeProc); ok {
		spn.UpdateBinary(binary, isCandidate)
	}
}

// UpdateGlobalBinary will set the 'bianry' to use on the orchestrator as a whole
func (e *Orchestrator) UpdateGlobalBinary(binary string, isCandidate bool) {
	e.binary = binary
	e.isBinaryCandidate = isCandidate
}

func (e *Orchestrator) CreateResharingGroup(oldToRemove, threshold int) {
	fmt.Println("[+] Setting up the nodes for the resharing")
	// create paths that contains old node + new nodes
	for _, n := range e.nodes[oldToRemove:] {
		fmt.Printf("\t- Adding current node %s\n", n.PrivateAddr())
		e.reshareIndex = append(e.reshareIndex, n.Index())
		e.reshareNodes = append(e.reshareNodes, n)
	}
	for _, n := range e.newNodes {
		fmt.Printf("\t- Adding new node %s\n", n.PrivateAddr())
		e.reshareIndex = append(e.reshareIndex, n.Index())
		e.reshareNodes = append(e.reshareNodes, n)
	}
	e.resharePaths = append(e.resharePaths, e.paths[oldToRemove:]...)
	e.resharePaths = append(e.resharePaths, e.newPaths...)
	e.newThr = threshold
	fmt.Printf("[+] Stopping old nodes\n")
	for _, n := range e.nodes {
		var found bool
		for _, idx := range e.reshareIndex {
			if idx == n.Index() {
				found = true
				break
			}
		}
		if !found {
			fmt.Printf("\t- Stopping old node %s\n", n.PrivateAddr())
			n.Stop()
		}
	}
}

func (e *Orchestrator) isNew(n node.Node) bool {
	for _, c := range e.newNodes {
		if c == n {
			return true
		}
	}
	return false
}

func (e *Orchestrator) RunResharing(timeout string) {
	fmt.Println("[+] Running DKG for resharing nodes")
	nodes := len(e.reshareNodes)
	thr := e.newThr
	groupCh := make(chan *key.Group, 1)
	leader := e.reshareNodes[0]
	panicCh := make(chan interface{}, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer func() {
			if err := recover(); err != nil {
				panicCh <- err
			}
		}()
		p := ""
		if e.isNew(leader) {
			p = e.groupPath
		}
		fmt.Printf("\t- Running DKG for leader node %s\n", leader.PrivateAddr())
		group := leader.RunReshare(nodes, thr, p, timeout, true, "", beaconOffset)
		fmt.Printf("\t- Resharing DONE for leader node %s\n", leader.PrivateAddr())
		wg.Done()
		groupCh <- group
	}()
	time.Sleep(100 * time.Millisecond)

	for _, n := range e.reshareNodes[1:] {
		n := n
		p := ""
		if e.isNew(n) {
			p = e.groupPath
		}
		fmt.Printf("\t- Running DKG for node %s\n", n.PrivateAddr())
		wg.Add(1)
		go func(n node.Node) {
			defer func() {
				if err := recover(); err != nil {
					wg.Done()
					panicCh <- err
				}
			}()
			n.RunReshare(nodes, thr, p, timeout, false, leader.PrivateAddr(), beaconOffset)
			fmt.Printf("\t- Resharing DONE for node %s\n", n.PrivateAddr())
			wg.Done()
		}(n)
	}
	wg.Wait()
	<-groupCh
	select {
	case p := <-panicCh:
		panic(p)
	default:
	}
	// we pass the new group file
	g := e.checkDKGNodes(e.reshareNodes, e.newGroupPath)
	e.newGroup = g
	e.transition = g.TransitionTime
	checkErr(key.Save(e.newGroupPath, e.newGroup, false))
	fmt.Println("\t- Overwrite reshared group with distributed key to ", e.newGroupPath)
	fmt.Println("[+] Check previous distributed key is the same as the new one")
	oldgroup := new(key.Group)
	newgroup := new(key.Group)
	checkErr(key.Load(e.groupPath, oldgroup))
	checkErr(key.Load(e.newGroupPath, newgroup))
	if !oldgroup.PublicKey.Key().Equal(newgroup.PublicKey.Key()) {
		fmt.Printf("[-] Invalid distributed key !\n")
	}
}

func createNodes(cfg cfg.Config) ([]node.Node, []string) {
	var nodes []node.Node
	for i := 0; i < cfg.N; i++ {
		idx := i + cfg.Offset
		var n node.Node
		if cfg.Binary != "" {
			n = node.NewNode(idx, cfg)
		} else {
			n = node.NewLocalNode(idx, "127.0.0.1", cfg)
		}
		n.WriteCertificate(path.Join(cfg.CertFolder, fmt.Sprintf("cert-%d", idx)))
		nodes = append(nodes, n)
		fmt.Printf("\t- Created node %s at %s --> ctrl port: %s\n", n.PrivateAddr(), cfg.BasePath, n.CtrlAddr())
	}
	// write public keys from all nodes
	var paths []string
	for _, nd := range nodes {
		p := path.Join(cfg.BasePath, fmt.Sprintf("public-%d.toml", nd.Index()))
		nd.WritePublic(p)
		paths = append(paths, p)
	}
	return nodes, paths
}

func (e *Orchestrator) StopNodes(idxs ...int) {
	for _, n := range e.nodes {
		for _, idx := range idxs {
			if n.Index() == idx {
				fmt.Printf("[+] Stopping node %s to simulate a node failure\n", n.PrivateAddr())
				n.Stop()
			}
		}
	}
}

func (e *Orchestrator) StopAllNodes(toExclude ...int) {
	filtered := filterNodes(e.nodes, toExclude...)
	fmt.Printf("[+] Stopping the rest (%d nodes) for a complete failure\n", len(filtered))
	for _, n := range filtered {
		e.StopNodes(n.Index())
	}
}

func (e *Orchestrator) StartNode(idxs ...int) {
	for _, idx := range idxs {
		var foundNode node.Node
		for _, n := range append(e.nodes, e.newNodes...) {
			if n.Index() == idx {
				foundNode = n
			}
		}
		if foundNode == nil {
			panic("node to start doesn't exist")
		}

		fmt.Printf("[+] Attempting to start node %s again ...\n", foundNode.PrivateAddr())
		// Here we send the nil values to the start method to allow the node to reconnect to the same database
		err := foundNode.Start(e.certFolder, "", nil, e.memDBSize)
		if err != nil {
			panic(fmt.Errorf("[-] Could not start node %s error: %v", foundNode.PrivateAddr(), err))
		}
		var started bool
		for trial := 1; trial < 10; trial += 1 {
			if foundNode.Ping() {
				fmt.Printf("\t- Node %s started correctly\n", foundNode.PrivateAddr())
				started = true
				break
			}
			time.Sleep(time.Duration(trial*trial) * time.Second)
		}
		if !started {
			panic(fmt.Errorf("[-] Could not start node %s", foundNode.PrivateAddr()))
		}
	}
}

func (e *Orchestrator) PrintLogs() {
	fmt.Println("[+] Printing logs for debugging on CI")
	for _, n := range e.nodes {
		n.PrintLog()
	}
	for _, n := range e.newNodes {
		n.PrintLog()
	}
}

func (e *Orchestrator) Shutdown() {
	fmt.Println("[+] Shutdown all nodes")
	for _, no := range e.nodes {
		fmt.Printf("\t- Stopping old node %s\n", no.PrivateAddr())
		go no.Stop()
	}
	for _, no := range e.newNodes {
		fmt.Printf("\t- Stopping new node %s\n", no.PrivateAddr())
		go no.Stop()
		fmt.Println("\t- Successfully stopped Node", no.Index(), "(", no.PrivateAddr(), ")")
	}
	fmt.Println("\t- Successfully sent Stop command to all node")
	time.Sleep(3 * time.Minute)
}

func runCommand(c *exec.Cmd, add ...string) []byte {
	out, err := c.CombinedOutput()
	if err != nil {
		if len(add) > 0 {
			fmt.Printf("[-] Msg failed command: %s\n", add[0])
		}
		fmt.Printf("[-] Command \"%s\" gave\n%s\n", strings.Join(c.Args, " "), string(out))
		panic(err)
	}
	return out
}

func checkErr(err error, out ...string) {
	if err == nil {
		return
	}
	if len(out) > 0 {
		panic(fmt.Errorf("%s: %v", out[0], err))
	}

	panic(err)
}

func pair(k, v string) []string {
	return []string{k, v}
}
