package server

import (
	"doozer/consensus"
	"doozer/proto"
	"doozer/store"
	"encoding/binary"
	"io"
	"log"
	"math"
	"net"
	"os"
	"rand"
	"sync"
	pb "goprotobuf.googlecode.com/hg/proto"
)


const packetSize = 3000


const (
	sessionLease = 6e9 // ns == 6s
	sessionPad   = 3e9 // ns == 3s
)


var (
	ErrPoisoned = os.NewError("poisoned")
)


var (
	badPath     = proto.NewResponse_Err(proto.Response_BAD_PATH)
	missingArg  = &R{ErrCode: proto.NewResponse_Err(proto.Response_MISSING_ARG)}
	tagInUse    = &R{ErrCode: proto.NewResponse_Err(proto.Response_TAG_IN_USE)}
	isDir       = &R{ErrCode: proto.NewResponse_Err(proto.Response_ISDIR)}
	notDir      = &R{ErrCode: proto.NewResponse_Err(proto.Response_NOTDIR)}
	noEnt       = &R{ErrCode: proto.NewResponse_Err(proto.Response_NOENT)}
	tooLate     = &R{ErrCode: proto.NewResponse_Err(proto.Response_TOO_LATE)}
	revMismatch = &R{ErrCode: proto.NewResponse_Err(proto.Response_REV_MISMATCH)}
	readonly    = &R{
		ErrCode:   proto.NewResponse_Err(proto.Response_OTHER),
		ErrDetail: pb.String("no known writeable addresses"),
	}
	badTag = &R{
		ErrCode:   proto.NewResponse_Err(proto.Response_OTHER),
		ErrDetail: pb.String("unknown tag"),
	}
)


func errResponse(e os.Error) *R {
	return &R{
		ErrCode:   proto.NewResponse_Err(proto.Response_OTHER),
		ErrDetail: pb.String(e.String()),
	}
}


// Response flags
const (
	Valid = 1 << iota
	Done
	Set
	Del
)


var calGlob = store.MustCompileGlob("/ctl/cal/*")


type T proto.Request
type R proto.Response


type OpError struct {
	Detail string
}


type Manager interface {
	consensus.Proposer
}


type Server struct {
	Addr string
	St   *store.Store
	Mg   Manager
	Self string

	Alpha int64
}


func (s *Server) accept(l net.Listener, ch chan net.Conn) {
	for {
		c, err := l.Accept()
		if err != nil {
			if err == os.EINVAL {
				break
			}
			if e, ok := err.(*net.OpError); ok && e.Error == os.EINVAL {
				break
			}
			log.Println(err)
			continue
		}
		ch <- c
	}
	close(ch)
}


func (s *Server) Serve(l net.Listener, cal chan bool) {
	var w bool
	conns := make(chan net.Conn)
	go s.accept(l, conns)
	for {
		select {
		case rw := <-conns:
			if closed(conns) {
				return
			}
			c := &conn{
				c:    rw,
				addr: rw.RemoteAddr().String(),
				s:    s,
				cal:  w,
				tx:   make(map[int32]txn),
			}
			go func() {
				c.serve()
				rw.Close()
			}()
		case <-cal:
			cal = nil
			w = true
		}
	}
}


func (sv *Server) cals() []string {
	cals := make([]string, 0)
	_, g := sv.St.Snap()
	store.Walk(g, calGlob, func(_, body string, _ int64) bool {
		if len(body) > 0 {
			cals = append(cals, body)
		}
		return false
	})
	return cals
}


// Repeatedly propose nop values until a successful read from `done`.
func (sv *Server) AdvanceUntil(done chan int) {
	for {
		select {
		case <-done:
			return
		default:
		}

		sv.Mg.Propose([]byte(store.Nop))
	}
}


func bgSet(p consensus.Proposer, k string, v []byte, c int64) chan store.Event {
	ch := make(chan store.Event)
	go func() {
		ch <- consensus.Set(p, k, v, c)
	}()
	return ch
}


func bgDel(p consensus.Proposer, k string, c int64) chan store.Event {
	ch := make(chan store.Event)
	go func() {
		ch <- consensus.Del(p, k, c)
	}()
	return ch
}


func bgNop(p consensus.Proposer) chan store.Event {
	ch := make(chan store.Event)
	go func() {
		ch <- p.Propose([]byte(store.Nop))
	}()
	return ch
}


type conn struct {
	c        io.ReadWriter
	wl       sync.Mutex // write lock
	addr     string
	s        *Server
	cal      bool
	sid      int32
	slk      sync.RWMutex
	tx       map[int32]txn
	tl       sync.Mutex // tx lock
	poisoned bool
}


var ops = map[int32]func(*conn, *T, txn){
	proto.Request_CANCEL: (*conn).cancel,
	proto.Request_DEL:    (*conn).del,
	proto.Request_GET:    (*conn).get,
	proto.Request_GETDIR: (*conn).getdir,
	proto.Request_NOP:    (*conn).nop,
	proto.Request_REV:    (*conn).rev,
	proto.Request_SET:    (*conn).set,
	proto.Request_STAT:   (*conn).stat,
	proto.Request_WALK:   (*conn).walk,
	proto.Request_WATCH:  (*conn).watch,
}


func (c *conn) readBuf() (*T, os.Error) {
	var size int32
	err := binary.Read(c.c, binary.BigEndian, &size)
	if err != nil {
		return nil, err
	}

	buf := make([]byte, size)
	_, err = io.ReadFull(c.c, buf)
	if err != nil {
		return nil, err
	}

	var t T
	err = pb.Unmarshal(buf, &t)
	if err != nil {
		return nil, err
	}
	return &t, nil
}


func (c *conn) serve() {
	defer c.cancelAll()

	for {
		t, err := c.readBuf()
		if err != nil {
			if err != os.EOF {
				log.Println(err)
			}
			return
		}

		verb := pb.GetInt32((*int32)(t.Verb))
		f, ok := ops[verb]
		if !ok {
			var r R
			r.ErrCode = proto.NewResponse_Err(proto.Response_UNKNOWN_VERB)
			c.respond(t, Valid|Done, nil, &r)
			continue
		}

		tag := pb.GetInt32((*int32)(t.Tag))
		tx := newTxn()

		c.tl.Lock()
		c.tx[tag] = tx
		c.tl.Unlock()

		f(c, t, tx)
	}
}


func (c *conn) closeTxn(tag int32) {
	c.tl.Lock()
	tx, ok := c.tx[tag]
	c.tx[tag] = txn{}, false
	c.tl.Unlock()
	if ok {
		close(tx.done)
	}
}


func (c *conn) respond(t *T, flag int32, cc chan bool, r *R) {
	r.Tag = t.Tag
	r.Flags = pb.Int32(flag)
	tag := pb.GetInt32(t.Tag)

	if flag&Done != 0 {
		c.closeTxn(tag)
	}

	if c.poisoned {
		select {
		case cc <- true:
		default:
		}
		return
	}

	buf, err := pb.Marshal(r)
	c.wl.Lock()
	defer c.wl.Unlock()
	if err != nil {
		c.poisoned = true
		select {
		case cc <- true:
		default:
		}
		log.Println(err)
		return
	}

	err = binary.Write(c.c, binary.BigEndian, int32(len(buf)))
	if err != nil {
		c.poisoned = true
		select {
		case cc <- true:
		default:
		}
		log.Println(err)
		return
	}

	for len(buf) > 0 {
		n, err := c.c.Write(buf)
		if err != nil {
			c.poisoned = true
			select {
			case cc <- true:
			default:
			}
			log.Println(err)
			return
		}

		buf = buf[n:]
	}
}


func (c *conn) redirect(t *T) {
	cals := c.s.cals()
	if len(cals) < 1 {
		c.respond(t, Valid|Done, nil, readonly)
		return
	}

	cal := cals[rand.Intn(len(cals))]
	parts, rev := c.s.St.Get("/ctl/node/" + cal + "/addr")
	if rev == store.Dir && rev == store.Missing {
		c.respond(t, Valid|Done, nil, readonly)
		return
	}

	r := &R{
		ErrCode:   proto.NewResponse_Err(proto.Response_REDIRECT),
		ErrDetail: &parts[0],
	}
	c.respond(t, Valid|Done, nil, r)
}


func (c *conn) getterFor(t *T) store.Getter {
	if t.Rev == nil {
		_, g := c.s.St.Snap()
		return g
	}

	ch, err := c.s.St.Wait(*t.Rev)
	switch err {
	default:
		c.respond(t, Valid|Done, nil, errResponse(err))
		return nil
	case store.ErrTooLate:
		c.respond(t, Valid|Done, nil, tooLate)
		return nil
	case nil:
		return (<-ch).Getter
	}

	panic("unreachable")
}


func (c *conn) get(t *T, tx txn) {
	if g := c.getterFor(t); g != nil {
		v, rev := g.Get(pb.GetString(t.Path))
		if rev == store.Dir {
			c.respond(t, Valid|Done, nil, isDir)
			return
		}

		var r R
		r.Rev = &rev
		if len(v) == 1 { // not missing
			r.Value = []byte(v[0])
		}
		c.respond(t, Valid|Done, nil, &r)
	}
}


func (c *conn) set(t *T, tx txn) {
	if !c.cal {
		c.redirect(t)
		return
	}

	if t.Path == nil || t.Rev == nil {
		c.respond(t, Valid|Done, nil, missingArg)
		return
	}

	go func() {
		select {
		case <-tx.cancel:
			c.closeTxn(*t.Tag)
			return
		case ev := <-bgSet(c.s.Mg, *t.Path, t.Value, *t.Rev):
			switch e := ev.Err.(type) {
			case *store.BadPathError:
				c.respond(t, Valid|Done, nil, &R{ErrCode: badPath, ErrDetail: &e.Path})
				return
			}

			switch ev.Err {
			default:
				c.respond(t, Valid|Done, nil, errResponse(ev.Err))
				return
			case store.ErrRevMismatch:
				c.respond(t, Valid|Done, nil, revMismatch)
				return
			case nil:
				c.respond(t, Valid|Done, nil, &R{Rev: &ev.Seqn})
				return
			}
		}

		panic("not reached")
	}()
}


func (c *conn) del(t *T, tx txn) {
	if !c.cal {
		c.redirect(t)
		return
	}

	if t.Path == nil || t.Rev == nil {
		c.respond(t, Valid|Done, nil, missingArg)
		return
	}

	go func() {
		select {
		case <-tx.cancel:
			c.closeTxn(*t.Tag)
			return
		case ev := <-bgDel(c.s.Mg, *t.Path, *t.Rev):
			if ev.Err != nil {
				c.respond(t, Valid|Done, nil, errResponse(ev.Err))
				return
			}
		}
		c.respond(t, Valid|Done, nil, &R{})
	}()
}


func (c *conn) nop(t *T, tx txn) {
	if !c.cal {
		c.redirect(t)
		return
	}

	go func() {
		select {
		case <-tx.cancel:
			c.closeTxn(*t.Tag)
			return
		case <-bgNop(c.s.Mg):
		}
		c.respond(t, Valid|Done, nil, &R{})
		return
	}()
}


func (c *conn) rev(t *T, tx txn) {
	rev := <-c.s.St.Seqns
	c.respond(t, Valid|Done, nil, &R{Rev: &rev})
}


func (c *conn) stat(t *T, tx txn) {
	if g := c.getterFor(t); g != nil {
		ln, rev := g.Stat(pb.GetString(t.Path))
		c.respond(t, Valid|Done, nil, &R{Len: &ln, Rev: &rev})
	}
}


func (c *conn) getdir(t *T, tx txn) {
	path := pb.GetString(t.Path)

	if g := c.getterFor(t); g != nil {
		go func() {
			ents, rev := g.Get(path)

			if rev == store.Missing {
				c.respond(t, Valid|Done, nil, noEnt)
				return
			}

			if rev != store.Dir {
				c.respond(t, Valid|Done, nil, notDir)
				return
			}

			offset := int(pb.GetInt32(t.Offset))
			limit := int(pb.GetInt32(t.Limit))

			if limit <= 0 {
				limit = len(ents)
			}

			if offset < 0 {
				offset = 0
			}

			end := offset + limit
			if end > len(ents) {
				end = len(ents)
			}

			for _, e := range ents[offset:end] {
				select {
				case <-tx.cancel:
					c.closeTxn(*t.Tag)
					return
				default:
				}

				c.respond(t, Valid, tx.cancel, &R{Path: &e})
			}

			c.respond(t, Done, nil, &R{})
		}()
	}
}


func (c *conn) cancelAll() {
	c.tl.Lock()
	for _, otx := range c.tx {
		select {
		case otx.cancel <- true:
		default:
		}
	}
	c.tl.Unlock()
}


func (c *conn) cancel(t *T, tx txn) {
	tag := pb.GetInt32(t.OtherTag)
	c.tl.Lock()
	otx, ok := c.tx[tag]
	c.tl.Unlock()
	if ok {
		select {
		case otx.cancel <- true:
		default:
		}
		<-otx.done
		c.respond(t, Valid|Done, nil, &R{})
	} else {
		c.respond(t, Valid|Done, nil, badTag)
	}
}


func (c *conn) watch(t *T, tx txn) {
	pat := pb.GetString(t.Path)
	glob, err := store.CompileGlob(pat)
	if err != nil {
		c.respond(t, Valid|Done, nil, errResponse(err))
		return
	}

	var w *store.Watch
	rev := pb.GetInt64(t.Rev)
	if rev == 0 {
		w, err = store.NewWatch(c.s.St, glob), nil
	} else {
		w, err = store.NewWatchFrom(c.s.St, glob, rev)
	}

	switch err {
	case nil:
		// nothing
	case store.ErrTooLate:
		c.respond(t, Valid|Done, nil, tooLate)
	default:
		c.respond(t, Valid|Done, nil, errResponse(err))
	}

	go func() {
		defer w.Stop()

		// TODO buffer (and possibly discard) events
		for {
			select {
			case ev := <-w.C:
				if closed(w.C) {
					return
				}

				r := R{
					Path:  &ev.Path,
					Value: []byte(ev.Body),
					Rev:   &ev.Seqn,
				}

				var flag int32
				switch {
				case ev.IsSet():
					flag = Set
				case ev.IsDel():
					flag = Del
				}

				c.respond(t, Valid|flag, tx.cancel, &r)

			case <-tx.cancel:
				c.closeTxn(*t.Tag)
				return
			}
		}
	}()
}


func (c *conn) walk(t *T, tx txn) {
	pat := pb.GetString(t.Path)
	glob, err := store.CompileGlob(pat)
	if err != nil {
		c.respond(t, Valid|Done, nil, errResponse(err))
		return
	}

	offset := pb.GetInt32(t.Offset)

	var limit int32 = math.MaxInt32
	if t.Limit != nil {
		limit = pb.GetInt32(t.Limit)
	}

	if g := c.getterFor(t); g != nil {
		go func() {
			f := func(path, body string, rev int64) (stop bool) {
				select {
				case <-tx.cancel:
					c.closeTxn(*t.Tag)
					return true
				default:
				}

				if offset <= 0 && limit > 0 {
					var r R
					r.Path = &path
					r.Value = []byte(body)
					r.Rev = &rev
					c.respond(t, Valid|Set, tx.cancel, &r)

					limit--
				}

				offset--
				return false
			}

			stopped := store.Walk(g, glob, f)

			if !stopped {
				c.respond(t, Done, nil, &R{})
			}
		}()
	}
}
