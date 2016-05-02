// Copyright 2016, Google
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package b2

//import (
//	"bytes"
//	"io"
//	"sync"
//
//	"github.com/kurin/gozer/base"
//	"golang.org/x/net/context"
//)
//
//type Reader struct {
//	ConcurrentDownloads int
//	ChunkSize           int
//
//	ctx    context.Context
//	cancel context.CancelFunc
//	bucket *base.Bucket
//	name   string
//	size   int64
//	csize  int
//	read   int
//	chwid  int
//	chrid  int
//	chbuf  chan *bytes.Buffer
//	init   sync.Once
//	rmux   sync.Mutex
//	rcond  *sync.Cond
//	chunks map[int]*bytes.Buffer
//
//	emux sync.RWMutex
//	err  error
//}
//
//func (r *Reader) Close() error {
//	r.cancel()
//	return nil
//}
//
//func (r *Reader) setErr(err error) {
//	r.emux.Lock()
//	defer r.emux.Unlock()
//	if r.err == nil {
//		r.err = err
//	}
//}
//
//func (r *Reader) getErr() error {
//	r.emux.RLock()
//	defer r.emux.RUnlock()
//	return r.err
//}
//
//func (r *Reader) thread() {
//	go func() {
//		for {
//			var buf *bytes.Buffer
//			select {
//			case b, ok := <-r.chbuf:
//				if !ok {
//					return
//				}
//				buf = b
//			case <-r.ctx.Done():
//				return
//			}
//			r.rmux.Lock()
//			chunkID := r.chwid
//			r.chwid++
//			r.rmux.Unlock()
//			offset := int64(chunkID * r.csize)
//			size := int64(r.csize)
//			if offset >= r.size {
//				return
//			}
//			if offset+size > r.size {
//				size = r.size - offset
//			}
//			fr, err := r.bucket.DownloadFileByName(r.ctx, r.name, offset, size)
//			if err != nil {
//				r.setErr(err)
//				r.rcond.Broadcast()
//				return
//			}
//			if _, err := copyContext(r.ctx, buf, fr); err != nil {
//				r.setErr(err)
//				r.rcond.Broadcast()
//				return
//			}
//			r.rmux.Lock()
//			r.chunks[chunkID] = buf
//			r.rmux.Unlock()
//			r.rcond.Broadcast()
//		}
//	}()
//}
//
//func (r *Reader) curChunk() (*bytes.Buffer, error) {
//	ch := make(chan *bytes.Buffer)
//	go func() {
//		r.rmux.Lock()
//		defer r.rmux.Unlock()
//		for r.chunks[r.chrid] == nil && r.getErr() == nil {
//			r.rcond.Wait()
//		}
//		select {
//		case ch <- r.chunks[r.chrid]:
//		case <-r.ctx.Done():
//			return
//		}
//	}()
//	select {
//	case buf := <-ch:
//		return buf, r.getErr()
//	case <-r.ctx.Done():
//		return nil, r.ctx.Err()
//	}
//}
//
//func (r *Reader) initFunc() {
//	r.rcond = sync.NewCond(&r.rmux)
//	cr := r.ConcurrentDownloads
//	if cr < 1 {
//		cr = 1
//	}
//	if r.ChunkSize < 1 {
//		r.ChunkSize = 1e7
//	}
//	r.csize = r.ChunkSize
//	r.chbuf = make(chan *bytes.Buffer, cr)
//	for i := 0; i < cr; i++ {
//		r.thread()
//		r.chbuf <- &bytes.Buffer{}
//	}
//}
//
//func (r *Reader) Read(p []byte) (int, error) {
//	r.init.Do(r.initFunc)
//	chunk, err := r.curChunk()
//	if err != nil {
//		return 0, err
//	}
//	n, err := chunk.Read(p)
//	r.read += n
//	if err == io.EOF {
//		if int64(r.read) >= r.size {
//			close(r.chbuf)
//			return n, err
//		}
//		r.chrid++
//		chunk.Reset()
//		r.chbuf <- chunk
//		err = nil
//	}
//	return n, err
//}
//
//// copied from io.Copy, basically.
//func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (written int64, err error) {
//	buf := make([]byte, 32*1024)
//	for {
//		if ctx.Err() != nil {
//			err = ctx.Err()
//			return
//		}
//		nr, er := src.Read(buf)
//		if nr > 0 {
//			nw, ew := dst.Write(buf[0:nr])
//			if nw > 0 {
//				written += int64(nw)
//			}
//			if ew != nil {
//				err = ew
//				break
//			}
//			if nr != nw {
//				err = io.ErrShortWrite
//				break
//			}
//		}
//		if er == io.EOF {
//			break
//		}
//		if er != nil {
//			err = er
//			break
//		}
//	}
//	return written, err
//}