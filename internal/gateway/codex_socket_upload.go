package gateway

import (
	"context"
	"errors"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

// A large create is still one text message. Split it into frames so a
// Pong can pass between writes without adding a round trip per chunk.
const codexUploadFrame = 16 << 10

func writeCodexUpload(ctx context.Context, c *websocket.Conn, data []byte) error {
	w, err := c.Writer(ctx, websocket.MessageText)
	if err != nil {
		return err
	}
	for len(data) > 0 {
		n := min(codexUploadFrame, len(data))
		if _, err := w.Write(data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	return w.Close()
}

type codexUploadFrameRead struct {
	typ  websocket.MessageType
	data []byte
	err  error
}

func (b *codexSocketBody) startRead() <-chan codexUploadFrameRead {
	next := make(chan codexUploadFrameRead, 1)
	go func() {
		typ, data, err := b.c.Read(b.ctx)
		next <- codexUploadFrameRead{typ, data, err}
	}()
	return next
}

// coder/websocket handles control frames only while reading. A metadata
// frame may arrive before the upload ends: keep it in order and resume
// the read so that subsequent Ping/Pong frames can still make progress.
func (b *codexSocketBody) upload(data []byte) error {
	sent := make(chan error, 1)
	b.reading = b.startRead()
	go func() { sent <- writeCodexUpload(b.ctx, b.c, data) }()
	size := 0
	for {
		select {
		case err := <-sent:
			return err
		case first := <-b.reading:
			b.reading = nil
			b.early = append(b.early, first)
			if first.err != nil {
				b.k.close()
				if err := <-sent; err != nil {
					return first.err
				}
				return nil // frame() reports the original close
			}
			size += len(first.data)
			switch gjson.GetBytes(first.data, "type").String() {
			case "response.completed", "response.failed", "response.incomplete", "response.done", "error":
				// The terminal frame belongs to this turn. Leave the next
				// read to the next owner if the socket goes into the pool.
				return <-sent
			}
			if len(b.early) >= 32 || size >= 1<<20 {
				// A real reply can race the final write. If it is done,
				// deliver the queued frames normally. Bound early data
				// from a peer speaking continuously before the create.
				select {
				case err := <-sent:
					return err
				default:
					b.k.close()
					<-sent
					return errors.New("codex websocket: too much reply data during upload")
				}
			}
			b.reading = b.startRead()
		}
	}
}

func (b *codexSocketBody) nextFrame() (websocket.MessageType, []byte, error) {
	var next codexUploadFrameRead
	switch {
	case len(b.early) > 0:
		next = b.early[0]
		b.early[0] = codexUploadFrameRead{}
		b.early = b.early[1:]
	case b.reading != nil:
		next = <-b.reading
		b.reading = nil
	default:
		return b.c.Read(b.ctx)
	}
	return next.typ, next.data, next.err
}
