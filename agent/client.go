package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"time"

	"github.com/guseggert/clustertest/agent/command"
	clusteriface "github.com/guseggert/clustertest/cluster"
	"go.uber.org/zap"
	"nhooyr.io/websocket"
)

type Client struct {
	Logger *zap.SugaredLogger

	host            string
	tlsClientConfig *tls.Config
	dialCtx         func(ctx context.Context, network, addr string) (net.Conn, error)
	baseURL         string
	httpClient      *http.Client
	commandClient   *command.Client
}

func NewClient(cert *Certs, ipAddr string, port int) (*Client, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	httpDialAddrPort := fmt.Sprintf("%s:%d", ipAddr, port)

	// Don't do DNS lookup for dialing.
	// This prevents the default dialer from modifying the host header, which we need since we are not using public CAs.
	// Resulting behavior is that the addr host is used for the host header, but it does not resolve the name.
	// Rationale is that we don't need TLS for server authn, since we control all the hosts anyway.
	// We just want authz and encryption.
	dialCtx := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", httpDialAddrPort)
	}

	tlsConfig, err := ClientTLSConfig(cert.CA.CertPEMBytes, cert.Client.CertPEMBytes, cert.Client.KeyPEMBytes)
	if err != nil {
		return nil, fmt.Errorf("building client TLS config: %w", err)
	}

	loggerCfg := zap.NewDevelopmentConfig()
	logger, err := loggerCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("building logger: %w", err)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext:     dialCtx,
			MaxConnsPerHost: 0,
			TLSClientConfig: tlsConfig,
		},
	}
	baseURL := fmt.Sprintf("https://nodeagent:%d", port)
	commandURL := baseURL + "/command"

	return &Client{
		Logger:          logger.Named("nodegaentclient").Sugar(),
		host:            "nodeagent",
		baseURL:         baseURL,
		httpClient:      httpClient,
		tlsClientConfig: tlsConfig,
		dialCtx:         dialCtx,
		commandClient: &command.Client{
			HTTPClient: httpClient,
			URL:        commandURL,
			Logger:     logger.Named("command_client").Sugar(),
		},
	}, nil
}

func (c *Client) prepReq(r *http.Request) {
	r.Header.Add("Content-Type", "application/json")
	r.Close = true
}

func (c *Client) SendHeartbeat(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	u := fmt.Sprintf(c.baseURL + "/heartbeat")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		panic(err)
	}

	c.prepReq(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP error: %w", err)
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected heartbeat status code %d", resp.StatusCode)
	}
	return nil

}

func (c *Client) SendFile(ctx context.Context, sendReq clusteriface.SendFileRequest) error {
	urlPath := path.Join("/file", sendReq.FilePath)
	u := c.baseURL + urlPath
	httpReq, err := http.NewRequest(http.MethodPost, u, sendReq.Contents)
	if err != nil {
		panic(err)
	}

	c.prepReq(httpReq)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("sending file over HTTP: %w", err)
	}
	if httpResp.Body != nil {
		defer httpResp.Body.Close()
	}
	if httpResp.StatusCode != http.StatusOK {
		var body string
		b, err := io.ReadAll(httpResp.Body)
		if err != nil {
			body = fmt.Errorf("error reading body: %w", err).Error()
		} else {
			body = string(b)
		}
		return fmt.Errorf("non-200 HTTP status code %d received when sending file: %s", httpResp.StatusCode, body)
	}
	return nil
}

func (c *Client) Run(ctx context.Context, runReq clusteriface.RunRequest) (clusteriface.RunResultWaiter, error) {
	wait, err := c.commandClient.Run(ctx, command.RunRequest{
		Command: runReq.Command,
		Args:    runReq.Args,
		Env:     runReq.Env,
		WD:      runReq.WD,
		Stdin:   runReq.Stdin,
		Stdout:  runReq.Stdout,
		Stderr:  runReq.Stderr,
	})
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (int, error) {
		res, err := wait(ctx)
		return res.Code, err
	}, nil
}

// Dial establishes a connection to the given address, using the node as a proxy.
func (c *Client) Dial(network, addr string) (net.Conn, error) {
	return c.DialContext(context.Background(), network, addr)
}

// DialContext establishes a connection to the given address using the given network type, tunneled through a WebSocket connection with the node.
func (c *Client) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	u := c.baseURL + fmt.Sprintf("/connect/%s/%s", network, addr)

	c.Logger.Debugw("dialing WebSocket", "URL", u)
	wsConn, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c.httpClient})
	if err != nil {
		return nil, fmt.Errorf("dialing WebSocket conn: %w", err)
	}

	return websocket.NetConn(ctx, wsConn, websocket.MessageBinary), nil
}

func (c *Client) WaitForServer(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			err := c.SendHeartbeat(ctx)
			if err == nil {
				c.Logger.Debug("heartbeat succeeded, done waiting for server")
				return nil
			}
			c.Logger.Debugf("got heartbeat error: %s", err)
		}
	}
}
