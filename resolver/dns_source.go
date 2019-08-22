package resolver

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"

	"github.com/chrisruffalo/gudgeon/util"
)

const (
	// default ports
	defaultPort    = uint(53)
	defaultTLSPort = uint(853)

	// string checking
	portDelimiter  = ":"
	protoDelimiter = "/"

	// how many workers to spawn
	minWorkers = 0
	// max workers to allow
	maxWorkers = 25
	// how many requests to buffer
	requestBuffer = 100
)

// how long a worker should stay up without work to do
var workerIdleTime = 10 * time.Second

// how long to wait before source is active again
var backoffInterval = 500 * time.Millisecond

// how long to wait before timing out the connection
var defaultDeadline = 1 * time.Second

var validProtocols = []string{"udp", "tcp", "tcp-tls"}

type dnsWork struct {
	message      *dns.Msg
	responseChan chan *dnsWorkResponse
}

type dnsWorkResponse struct {
	err      error
	response *dns.Msg
}

type dnsSource struct {
	dnsServer     string
	port          uint
	remoteAddress string
	protocol      string
	network       string

	dialer net.Dialer

	backoffTime *time.Time
	tlsConfig   *tls.Config

	// are we closing?
	closing bool
	// used to buffer  incoming requests (work)
	questionChan chan *dnsWork
	// used to close individual workers
	closeChan chan bool

	workers     int
	workerGroup sync.WaitGroup

	sourceChanMtx sync.RWMutex
}

func (source *dnsSource) Name() string {
	return source.remoteAddress + "/" + source.protocol
}

func (source *dnsSource) Load(specification string) {
	source.port = 0
	source.dnsServer = ""
	source.protocol = ""

	// determine first if there is an attached protocol
	if strings.Contains(specification, protoDelimiter) {
		split := strings.Split(specification, protoDelimiter)
		if len(split) > 1 && util.StringIn(strings.ToLower(split[1]), validProtocols) {
			specification = split[0]
			source.protocol = strings.ToLower(split[1])
		}
	}

	// need to determine if a port comes along with the address and parse it out once
	if strings.Contains(specification, portDelimiter) {
		split := strings.Split(specification, portDelimiter)
		if len(split) > 1 {
			source.dnsServer = split[0]
			var err error
			parsePort, err := strconv.ParseUint(split[1], 10, 32)
			// recover from error
			if err != nil {
				source.port = 0
			} else {
				source.port = uint(parsePort)
			}
		}
	} else {
		source.dnsServer = specification
	}

	// set defaults if missing
	if "" == source.protocol {
		source.protocol = "udp"
	}
	// the network should be just tcp, really
	source.network = source.protocol
	if "tcp-tls" == source.protocol {
		source.network = "tcp"
	}

	// recover from parse errors or use default port in event port wasn't set
	if source.port == 0 {
		if "tcp-tls" == source.protocol {
			source.port = defaultTLSPort
		} else {
			source.port = defaultPort
		}
	}

	// set up tls config
	source.tlsConfig = &tls.Config{InsecureSkipVerify: true}

	// check final output
	if ip := net.ParseIP(source.dnsServer); ip != nil {
		// save/parse remote address once
		source.remoteAddress = fmt.Sprintf("%s%s%d", source.dnsServer, portDelimiter, source.port)
	}

	// keep dialer for reuse
	source.dialer = net.Dialer{}
	// set tcp dialer properties
	source.dialer.Timeout = time.Second * 2
	source.dialer.KeepAlive = 0

	// create com channels and start (empty) worker pool
	source.workers = 0
	source.questionChan = make(chan *dnsWork, requestBuffer)
	source.closeChan = make(chan bool, maxWorkers*2) // max workers udp + tcp
}

func (source *dnsSource) connect() (*dns.Conn, error) {
	conn, err := source.dialer.Dial(source.network, source.remoteAddress)
	if err != nil {
		return nil, err
	}
	if source.protocol == "tcp-tls" {
		conn = tls.Client(conn, source.tlsConfig)
	}
	return &dns.Conn{Conn: conn}, nil
}

func (source *dnsSource) handle(co *dns.Conn, request *dns.Msg) (*dns.Msg, error) {
	// update deadline waiting for write to succeed
	_ = co.SetDeadline(time.Now().Add(defaultDeadline))

	// write message
	if err := co.WriteMsg(request); err != nil {
		return nil, err
	}

	// read response with deadline
	_ = co.SetDeadline(time.Now().Add(2 * defaultDeadline))
	response, err := co.ReadMsg()
	if err != nil {
		return nil, err
	}

	return response, nil
}

func (source *dnsSource) udpWorker(idleTimer *time.Timer) {
	for true {
		select {
		case <-source.closeChan:
			log.Tracef("Closing '%s' udp worker", source.Name())
			return
		case <-idleTimer.C:
			// since udp doesn't keep connections just break free here
			return
		case work := <-source.questionChan:
			if !source.closing {
				// activity means reset timer, even if there are later errors
				// the thread itself was needed to service a request
				idleTimer.Reset(workerIdleTime)

				co, err := source.connect()
				if err != nil {
					work.responseChan <- &dnsWorkResponse{err, nil}
				} else {
					response, err := source.handle(co, work.message)
					work.responseChan <- &dnsWorkResponse{err, response}
				}
			} else {
				if work != nil && work.responseChan != nil {
					work.responseChan <- &dnsWorkResponse{nil, nil}
				}
				return
			}
		}
	}
}

func (source *dnsSource) tcpWorker(idleTimer *time.Timer) {
	co, err := source.connect()
	if err != nil {
		log.Errorf("Could not establish %s connection: %s", source.protocol, err)
	}

	for true {
		select {
		// closed by source closing
		case <-source.closeChan:
			if co != nil {
				err = co.Close()
				if err != nil {
					// this means something was in flight as the connection was being
					// closed and there is very little we can do at that point
					log.Debugf("Could not close connection: %s", err)
				}
			}
			log.Tracef("Closing '%s' tcp worker", source.Name())
			return
		// closed by expiring timer
		case <-idleTimer.C:
			if co != nil {
				err = co.Close()
				if err != nil {
					// this shouldn't really be the case as only one thread
					// can use the connection at a time through the virtue
					// of the channel selector so it'd be hard for something
					// to be "in the pipe" when it closes
					log.Debugf("Could not close connection: %s", err)
				}
			}
			return
		case work := <-source.questionChan:
			if source.closing {
				_ = co.Close()
				work.responseChan <- &dnsWorkResponse{nil, nil}
				return
			} else {
				// in the event of any activity the idle timer gets reset
				idleTimer.Reset(workerIdleTime)
				// if the connection is nil create a new one
				if co == nil {
					log.Tracef("opening new tcp connection in worker")
					co, err = source.connect()
					if err != nil {
						work.responseChan <- &dnsWorkResponse{err, nil}
						if co != nil {
							_ = co.Close()
						}
						co = nil
					}
				}
				// if we have a connection then use it
				if co != nil {
					response, err := source.handle(co, work.message)
					// reopen connection on error
					if err != nil {
						_ = co.Close()
						co = nil
						// if eof or broken pipe it probably just means we held on to the connection too long
						// and we can just reopen it and try again
						if nErr, ok := err.(*net.OpError); (ok && nErr.Err == syscall.EPIPE) || err == io.EOF {
							co, err = source.connect()
							if err != nil {
								// reset connection we can't make anyway and keep error for returning over channel
								co = nil
							} else {
								// actually handle the request with the given connection
								// and then use the response and/or error in output
								response, err = source.handle(co, work.message)
							}
						}
					}
					// if no response was given or the error is not nil we can still return it
					if work != nil && work.responseChan != nil {
						work.responseChan <- &dnsWorkResponse{err, response}
					}
				}
			}
		}
	}
}

func (source *dnsSource) worker() {
	// add to wait group
	source.workerGroup.Add(1)

	// create timer
	idleTimer := time.NewTimer(workerIdleTime)

	// spawn appropriate worker
	if source.protocol == "udp" {
		source.udpWorker(idleTimer)
	} else {
		source.tcpWorker(idleTimer)
	}

	// stop timer
	idleTimer.Stop()

	// done
	source.workerGroup.Done()
}

func (source *dnsSource) increaseWorkers() {
	// if there are no workers, spawn one, otherwise...
	// use pressure to decide to spawn more workers if the request buffer is more than the fraction
	// full and the workers is less than the number of max allowed workers
	if source.workers <= minWorkers || (source.workers <= maxWorkers && len(source.questionChan) > requestBuffer/2) {
		go source.worker()
		source.workers++
	}
}

func (source *dnsSource) decreaseWorkers() {
	// attempt to use the same pressure concept to reduce the number of workers if the queue length is
	// less than the fraction
	if source.workers > minWorkers && len(source.questionChan) < requestBuffer/2 {
		source.closeChan <- true
		source.workers--
	}
}

func (source *dnsSource) query(request *dns.Msg) (*dns.Msg, error) {
	source.sourceChanMtx.RLock()
	if source.questionChan == nil {
		defer source.sourceChanMtx.RUnlock()
		return nil, fmt.Errorf("Resolver source '%s' closed", source.Name())
	}
	source.sourceChanMtx.RUnlock()

	responseChan := make(chan *dnsWorkResponse)
	source.questionChan <- &dnsWork{request, responseChan}
	answer := <-responseChan
	close(responseChan)
	return answer.response, answer.err
}

func (source *dnsSource) Answer(rCon *RequestContext, context *ResolutionContext, request *dns.Msg) (*dns.Msg, error) {
	now := time.Now()
	if source.backoffTime != nil && now.Before(*source.backoffTime) {
		// "asleep" during backoff interval
		return nil, nil
	}
	// the backoff time is irrelevant now
	source.backoffTime = nil

	// this is considered a recursive query so don't if recursion was not requested
	if request == nil || !request.MsgHdr.RecursionDesired {
		return nil, nil
	}

	// check and increase pressure before submitting, this is an async call so
	// it will not slow things down, however reducing pressure in this thread
	// would have to wait for the "close" message to be received which is sync
	// so not done in the main execution path and is instead delegated to a monitor
	// thread with a timer
	source.increaseWorkers()

	// forward message without interference
	response, err := source.query(request)

	// now respond to error after deciding what to do about the number of routines
	if err != nil {
		backoff := time.Now().Add(backoffInterval)
		source.backoffTime = &backoff
		return nil, err
	}
	// do not set reply here (doesn't seem to matter, leaving this comment so nobody decides to do it in the future without cause)
	// response.SetReply(request)

	// set source as answering source
	if context != nil && !util.IsEmptyResponse(response) && context.SourceUsed == "" {
		context.SourceUsed = source.Name()
	}

	// otherwise just return
	return response, nil
}

func (source *dnsSource) Close() {
	// start by setting closing to true
	source.closing = true

	// stop pressure modifier and wait for thread to close
	log.Debugf("Closing dns source: %s", source.Name())
	// send enough messages to stop workers
	for i := 0; i < maxWorkers; i++ {
		source.closeChan <- true
	}

	// close input channel
	source.sourceChanMtx.Lock()
	close(source.questionChan)
	source.questionChan = nil
	source.sourceChanMtx.Unlock()

	// wait for workers to close
	source.workerGroup.Wait()

	// close response chan
	close(source.closeChan)
}
