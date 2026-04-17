//go:build linux

package proxy

import (
	"context"
	"fmt"
	"net"
	"runtime"

	vishnetns "github.com/vishvananda/netns"
)

// NsDialer 在目标命名空间中建立出站连接。
type NsDialer struct{}

// DialInNs 切入目标命名空间后拨号，再切回原始命名空间。
func (d *NsDialer) DialInNs(ctx context.Context, nsHandle vishnetns.NsHandle, network, address string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}

	resultCh := make(chan result, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		origin, err := vishnetns.Get()
		if err != nil {
			resultCh <- result{err: fmt.Errorf("get current namespace: %w", err)}
			return
		}
		defer origin.Close()

		if err := vishnetns.Set(nsHandle); err != nil {
			resultCh <- result{err: fmt.Errorf("switch to target namespace: %w", err)}
			return
		}

		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, network, address)

		if resetErr := vishnetns.Set(origin); resetErr != nil && err == nil {
			err = fmt.Errorf("restore original namespace: %w", resetErr)
		}

		resultCh <- result{conn: conn, err: err}
	}()

	select {
	case result := <-resultCh:
		return result.conn, result.err
	case <-ctx.Done():
		// ctx 已取消，但 goroutine 可能仍在 dial。启动兜底协程：
		// 如果后续收到 conn，及时 Close 防止泄漏。
		go func() {
			if late := <-resultCh; late.conn != nil {
				_ = late.conn.Close()
			}
		}()
		return nil, ctx.Err()
	}
}
