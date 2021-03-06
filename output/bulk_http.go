package output

import (
	"bytes"
	"compress/gzip"
	"context"
	"io/ioutil"
	"math/rand"
	"net/http"
	"regexp"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/golang/glog"
)

const (
	DEFAULT_BULK_SIZE      = 15 * 1024 * 1024
	DEFAULT_BULK_ACTIONS   = 5000
	DEFAULT_FLUSH_INTERVAL = 30
	DEFAULT_CONCURRENT     = 1

	MAX_BYTE_SIZE_APPLIED_IN_ADVANCE = 1024 * 1024 * 50
)

var (
	REMOVE_HTTP_AUTH_REGEXP = regexp.MustCompile(`^(?i)(http(s?)://)[^:]+:[^@]+@`)
)

type HostSelector interface {
	selectOneHost() string
	reduceWeight(string)
	addWeight(string)
}

type RRHostSelector struct {
	hosts      []string
	initWeight int
	weight     []int
	index      int
	hostsCount int
}

func NewRRHostSelector(hosts []string, weight int) *RRHostSelector {
	rand.Seed(time.Now().UnixNano())
	hostsCount := len(hosts)
	rst := &RRHostSelector{
		hosts:      hosts,
		index:      int(rand.Int31n(int32(hostsCount))),
		hostsCount: hostsCount,
		initWeight: weight,
	}
	rst.weight = make([]int, hostsCount)
	for i := 0; i < hostsCount; i++ {
		rst.weight[i] = weight
	}

	return rst
}

func (s *RRHostSelector) selectOneHost() string {
	// reset weight and return "" if all hosts are down
	var hasAtLeastOneUp bool = false
	for i := 0; i < s.hostsCount; i++ {
		if s.weight[i] > 0 {
			hasAtLeastOneUp = true
		}
	}
	if !hasAtLeastOneUp {
		s.resetWeight(s.initWeight)
		return ""
	}

	s.index = (s.index + 1) % s.hostsCount
	return s.hosts[s.index]
}

func (s *RRHostSelector) resetWeight(weight int) {
	for i := range s.weight {
		s.weight[i] = weight
	}
}

func (s *RRHostSelector) reduceWeight(host string) {
	for i, h := range s.hosts {
		if host == h {
			s.weight[i] = s.weight[i] - 1
			if s.weight[i] < 0 {
				s.weight[i] = 0
			}
			return
		}
	}
}

func (s *RRHostSelector) addWeight(host string) {
	for i, h := range s.hosts {
		if host == h {
			s.weight[i] = s.weight[i] + 1
			if s.weight[i] > s.initWeight {
				s.weight[i] = s.initWeight
			}
			return
		}
	}
}

type Event interface {
	Encode() []byte
}

type BulkRequest interface {
	add(Event)
	bufSizeByte() int
	eventCount() int
	readBuf() []byte
}
type NewBulkRequestFunc func() BulkRequest

type BulkProcessor interface {
	add(Event)
	bulk(BulkRequest, int)
	awaitclose(time.Duration)
}

type GetRetryEventsFunc func(*http.Response, []byte, BulkRequest) ([]int, []int, BulkRequest)

type HTTPBulkProcessor struct {
	headers           map[string]string
	requestMethod     string
	retryResponseCode map[int]bool
	bulk_size         int
	bulk_actions      int
	flush_interval    int
	concurrent        int
	compress          bool
	execution_id      int
	client            *http.Client
	mux               sync.Mutex
	wg                sync.WaitGroup
	semaphore         *semaphore.Weighted

	hostSelector       HostSelector
	bulkRequest        BulkRequest
	newBulkRequestFunc NewBulkRequestFunc
	getRetryEventsFunc GetRetryEventsFunc
}

func NewHTTPBulkProcessor(headers map[string]string, hosts []string, requestMethod string, retryResponseCode map[int]bool, bulk_size, bulk_actions, flush_interval, concurrent int, compress bool, newBulkRequestFunc NewBulkRequestFunc, getRetryEventsFunc GetRetryEventsFunc) *HTTPBulkProcessor {
	bulkProcessor := &HTTPBulkProcessor{
		headers:            headers,
		requestMethod:      requestMethod,
		retryResponseCode:  retryResponseCode,
		bulk_size:          bulk_size,
		bulk_actions:       bulk_actions,
		flush_interval:     flush_interval,
		client:             &http.Client{},
		hostSelector:       NewRRHostSelector(hosts, 3),
		concurrent:         concurrent,
		compress:           compress,
		bulkRequest:        newBulkRequestFunc(),
		newBulkRequestFunc: newBulkRequestFunc,
		getRetryEventsFunc: getRetryEventsFunc,
	}
	bulkProcessor.semaphore = semaphore.NewWeighted(int64(concurrent))

	ticker := time.NewTicker(time.Second * time.Duration(flush_interval))
	go func() {
		for range ticker.C {
			bulkProcessor.semaphore.Acquire(context.TODO(), 1)
			bulkProcessor.mux.Lock()
			if bulkProcessor.bulkRequest.eventCount() == 0 {
				bulkProcessor.mux.Unlock()
				bulkProcessor.semaphore.Release(1)
				continue
			}
			bulkRequest := bulkProcessor.bulkRequest
			bulkProcessor.bulkRequest = newBulkRequestFunc()
			bulkProcessor.execution_id++
			execution_id := bulkProcessor.execution_id
			bulkProcessor.mux.Unlock()
			bulkProcessor.bulk(bulkRequest, execution_id)
		}
	}()

	return bulkProcessor
}

func (p *HTTPBulkProcessor) add(event Event) {
	p.bulkRequest.add(event)

	// TODO bulkRequest passed to bulk may be empty, but execution_id has ++
	if p.bulkRequest.bufSizeByte() >= p.bulk_size || p.bulkRequest.eventCount() >= p.bulk_actions {
		p.semaphore.Acquire(context.TODO(), 1)
		p.mux.Lock()
		if p.bulkRequest.eventCount() == 0 {
			p.mux.Unlock()
			p.semaphore.Release(1)
			return
		}
		bulkRequest := p.bulkRequest
		p.bulkRequest = p.newBulkRequestFunc()
		p.execution_id++
		execution_id := p.execution_id
		p.mux.Unlock()
		go p.bulk(bulkRequest, execution_id)
	}
}

func (p *HTTPBulkProcessor) awaitclose(timeout time.Duration) {
	c := make(chan bool)
	defer func() {
		select {
		case <-c:
			glog.Info("all bulk job done. return")
			return
		case <-time.After(timeout):
			glog.Info("await timeout. return")
			return
		}
	}()

	defer func() {
		go func() {
			p.wg.Wait()
			c <- true
		}()
	}()

	p.mux.Lock()
	if p.bulkRequest.eventCount() == 0 {
		p.mux.Unlock()
		return
	}
	bulkRequest := p.bulkRequest
	p.bulkRequest = p.newBulkRequestFunc()
	p.execution_id++
	execution_id := p.execution_id
	p.mux.Unlock()

	p.wg.Add(1)
	go func() {
		p.innerBulk(bulkRequest, execution_id)
		p.wg.Done()
	}()
}

func (p *HTTPBulkProcessor) bulk(bulkRequest BulkRequest, execution_id int) {
	defer p.wg.Done()
	defer p.semaphore.Release(1)
	p.wg.Add(1)
	if bulkRequest.eventCount() == 0 {
		return
	}
	p.innerBulk(bulkRequest, execution_id)
}

func (p *HTTPBulkProcessor) innerBulk(bulkRequest BulkRequest, execution_id int) {
	_startTime := float64(time.Now().UnixNano()/1000000) / 1000
	eventCount := bulkRequest.eventCount()
	glog.Infof("bulk %d docs with execution_id %d", eventCount, execution_id)
	for {
		host := p.hostSelector.selectOneHost()
		if host == "" {
			glog.Info("no available host, wait for 30s")
			time.Sleep(30 * time.Second)
			continue
		}

		glog.Infof("try to bulk with host (%s)", REMOVE_HTTP_AUTH_REGEXP.ReplaceAllString(host, "${1}"))

		url := host
		success, shouldRetry, noRetry, newBulkRequest := p.tryOneBulk(url, bulkRequest)
		if success {
			_finishTime := float64(time.Now().UnixNano()/1000000) / 1000
			timeTaken := _finishTime - _startTime
			glog.Infof("bulk done with execution_id %d %.3f %d %.3f", execution_id, timeTaken, eventCount, float64(eventCount)/timeTaken)
			p.hostSelector.addWeight(host)
		} else {
			glog.Errorf("bulk failed with %s", url)
			p.hostSelector.reduceWeight(host)
			continue
		}

		if len(shouldRetry) > 0 || len(noRetry) > 0 {
			glog.Infof("%d should retry; %d need not retry", len(shouldRetry), len(noRetry))
		}

		if len(shouldRetry) > 0 {
			p.mux.Lock()
			p.execution_id++
			execution_id := p.execution_id
			p.mux.Unlock()
			p.innerBulk(newBulkRequest, execution_id)
		}

		return // only success will go to here
	}
}

func (p *HTTPBulkProcessor) tryOneBulk(url string, br BulkRequest) (bool, []int, []int, BulkRequest) {
	glog.V(5).Infof("request size:%d", br.bufSizeByte())
	glog.V(20).Infof("%s", br.readBuf())

	var (
		shouldRetry    = make([]int, 0)
		noRetry        = make([]int, 0)
		err            error
		req            *http.Request
		newBulkRequest BulkRequest
	)

	if p.compress {
		var buf bytes.Buffer
		g := gzip.NewWriter(&buf)
		if _, err = g.Write(br.readBuf()); err != nil {
			glog.Errorf("gzip bulk buf error: %s", err)
			return false, shouldRetry, noRetry, nil
		}
		if err = g.Close(); err != nil {
			glog.Errorf("gzip bulk buf error: %s", err)
			return false, shouldRetry, noRetry, nil
		}
		req, err = http.NewRequest(p.requestMethod, url, &buf)
		req.Header.Set("Content-Encoding", "gzip")
	} else {
		req, err = http.NewRequest(p.requestMethod, url, bytes.NewBuffer(br.readBuf()))
	}

	if err != nil {
		glog.Errorf("create request error: %s", err)
		return false, shouldRetry, noRetry, nil
	}

	for k, v := range p.headers {
		req.Header.Set(k, v)
	}

	resp, err := p.client.Do(req)

	if err != nil {
		glog.Infof("request with %s error: %s", url, err)
		return false, shouldRetry, noRetry, nil
	}

	defer resp.Body.Close()

	if p.retryResponseCode[resp.StatusCode] {
		return false, shouldRetry, noRetry, nil
	}

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		glog.Errorf(`read bulk response error: %s. will NOT retry`, err)
		return true, shouldRetry, noRetry, nil
	}
	glog.V(5).Infof("get response[%d]", len(respBody))
	glog.V(20).Infof("%s", respBody)

	shouldRetry, noRetry, newBulkRequest = p.getRetryEventsFunc(resp, respBody, br)

	return true, shouldRetry, noRetry, newBulkRequest
}
