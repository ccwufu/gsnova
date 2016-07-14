package main

import (
	//"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/yinqiwen/gsnova/common/event"
)

var proxySessionMap map[SessionId]*ProxySession = make(map[SessionId]*ProxySession)
var sessionMutex sync.Mutex

type SessionId struct {
	User      string
	Id        uint32
	ConnIndex int
}

type ProxySession struct {
	Id         SessionId
	CreateTime time.Time
	conn       net.Conn
	addr       string
}

func getProxySessionByEvent(user string, idx int, ev event.Event) *ProxySession {
	sid := SessionId{user, ev.GetId(), idx}
	sessionMutex.Lock()
	defer sessionMutex.Unlock()
	if p, exist := proxySessionMap[sid]; exist {
		return p
	}
	createIfMissing := false
	switch ev.(type) {
	case *event.TCPOpenEvent:
		createIfMissing = true
	case *event.HTTPRequestEvent:
		createIfMissing = true
	}
	if !createIfMissing {
		return nil
	}
	p := new(ProxySession)
	p.Id = sid
	p.CreateTime = time.Now()
	proxySessionMap[sid] = p
	return p
}

func removeProxySession(s *ProxySession) {
	sessionMutex.Lock()
	defer sessionMutex.Unlock()
	delete(proxySessionMap, s.Id)
	log.Printf("Remove sesion:%d, %d left", s.Id.Id, len(proxySessionMap))
}

func removeProxySessionsByConn(user string, connIndex int) {
	sessionMutex.Lock()
	defer sessionMutex.Unlock()
	for k, s := range proxySessionMap {
		if k.User == user && k.ConnIndex == connIndex {
			s.close()
			delete(proxySessionMap, k)
		}
	}
}

func (p *ProxySession) publish(ev event.Event) {
	ev.SetId(p.Id.Id)
	start := time.Now()
	for {
		queue := getEventQueue(p.Id.User, p.Id.ConnIndex, false)
		if nil != queue {
			queue.Publish(ev)
			return
		}
		if time.Now().After(start.Add(5 * time.Second)) {
			log.Printf("No avaliable connection to write event")
			p.close()
			break
		}
		time.Sleep(1 * time.Millisecond)
	}
}

func (p *ProxySession) close() error {
	if nil != p.conn {
		log.Printf("Session[%s:%d] close connection to %s", p.Id.User, p.Id.Id, p.addr)
		p.conn.Close()
		p.conn = nil
		p.addr = ""
	}
	return nil
}

func (p *ProxySession) initialClose() {
	ev := &event.TCPCloseEvent{}
	p.publish(ev)
	p.close()
	removeProxySession(p)
}

// func (p *ProxySession) httpInvoke(req *http.Request) error {
// 	p.request = req
// 	res, err := http.DefaultClient.Do(req)
// 	if nil != err {
// 		log.Printf("%v", err)
// 		ev := &event.TCPCloseEvent{}
// 		p.publish(ev)
// 		return err
// 	}
// 	p.response = res
// 	ev := event.NewHTTPResponseEvent(res)
// 	p.publish(ev)
// 	for nil != res.Body {
// 		buffer := make([]byte, 8192)
// 		n, err := res.Body.Read(buffer)
// 		if nil != err {
// 			break
// 		}
// 		var chunk event.TCPChunkEvent
// 		chunk.Content = buffer[0:n]
// 		p.publish(&chunk)
// 	}
// 	return nil
// }

func (p *ProxySession) open(to string) error {
	if p.conn != nil && to == p.addr {
		return nil
	}
	p.close()
	log.Printf("Session[%s:%d] open connection to %s.", p.Id.User, p.Id.Id, to)
	c, err := net.DialTimeout("tcp", to, 5*time.Second)
	if nil != err {
		ev := &event.TCPCloseEvent{}
		p.publish(ev)
		log.Printf("###Failed to connect %s for reason:%v", to, err)
		return err
	}
	p.conn = c
	p.addr = to
	go p.readTCP()
	return nil
}

func (p *ProxySession) write(b []byte) (int, error) {
	if p.conn == nil {
		log.Printf("Session[%s:%d] have no established connection to %s.", p.Id.User, p.Id.Id, p.addr)
		p.initialClose()
		return 0, nil
	}
	n, err := p.conn.Write(b)
	if nil != err {
		p.initialClose()
	}
	return n, err
}

func (p *ProxySession) readTCP() error {
	for {
		if nil == p.conn {
			return nil
		}
		p.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		b := make([]byte, 8192)
		n, err := p.conn.Read(b)
		if nil != err {
			p.initialClose()
			return err
		}
		ev := &event.TCPChunkEvent{Content: b[0:n]}
		p.publish(ev)
	}
	return nil
}

func (p *ProxySession) handle(ev event.Event) error {
	//p.evQueue.Publish(ev)
	switch ev.(type) {
	case *event.TCPOpenEvent:
		return p.open(ev.(*event.TCPOpenEvent).Addr)
	case *event.TCPCloseEvent:
		p.close()
		removeProxySession(p)
	case *event.TCPChunkEvent:
		p.write(ev.(*event.TCPChunkEvent).Content)
	case *event.HTTPRequestEvent:
		req := ev.(*event.HTTPRequestEvent)
		addr := req.Headers.Get("Host")
		if !strings.Contains(addr, ":") {
			if !strings.EqualFold("Connect", req.Method) {
				addr = addr + ":80"
			} else {
				addr = addr + ":443"
			}
		}
		log.Printf("Session[%d] %s %s", ev.GetId(), req.Method, req.URL)
		err := p.open(addr)
		if nil != err {
			return err
		}
		content := req.HTTPEncode()
		_, err = p.write(content)
		return err
	default:
		log.Printf("Invalid event type:%T to process", ev)
	}
	return nil
}