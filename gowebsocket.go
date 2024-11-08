package gowebsocket

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	logging "github.com/sacOO7/go-logger"
)

// Empty struct for logger initialization
type Empty struct{}

// Initialize logger
var logger = logging.GetLogger(reflect.TypeOf(Empty{}).PkgPath()).SetLevel(logging.OFF)

// Enable logging
func (socket Socket) EnableLogging() {
	logger.SetLevel(logging.TRACE)
}

// Get the logger instance
func (socket Socket) GetLogger() logging.Logger {
	return logger
}

// Socket structure
type Socket struct {
	Conn              *websocket.Conn
	WebsocketDialer   *websocket.Dialer
	Url               string
	ConnectionOptions ConnectionOptions
	RequestHeader     http.Header
	OnConnected       func(socket Socket)
	OnTextMessage     func(message string, socket Socket)
	OnBinaryMessage   func(data []byte, socket Socket)
	OnConnectError    func(err error, socket Socket)
	OnDisconnected    func(err error, socket Socket)
	OnPingReceived    func(data string, socket Socket)
	OnPongReceived    func(data string, socket Socket)
	IsConnected       bool
	Timeout           time.Duration
	sendMu            sync.Mutex     // Mutex to prevent concurrent writes
	receiveMu         sync.Mutex     // Mutex to prevent concurrent reads
	connStateMu       sync.Mutex     // Mutex to protect connection state
	closeChan         chan struct{}  // Channel to signal closing
	closeWg           sync.WaitGroup // WaitGroup to wait for goroutines
}

// Connection options structure
type ConnectionOptions struct {
	UseCompression bool
	UseSSL         bool
	Proxy          func(*http.Request) (*url.URL, error)
	Subprotocols   []string
}

// Reconnection options (to be implemented)
type ReconnectionOptions struct {
	// Fields for reconnection options
}

// Create a new Socket instance
func New(url string) Socket {
	return Socket{
		Url:           url,
		RequestHeader: http.Header{},
		ConnectionOptions: ConnectionOptions{
			UseCompression: false,
			UseSSL:         true,
		},
		WebsocketDialer: &websocket.Dialer{},
		Timeout:         0,
		closeChan:       make(chan struct{}),
		// Other fields are zero-initialized
	}
}

// Set connection options based on the configuration
func (socket *Socket) setConnectionOptions() {
	socket.WebsocketDialer.EnableCompression = socket.ConnectionOptions.UseCompression
	socket.WebsocketDialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: socket.ConnectionOptions.UseSSL}
	socket.WebsocketDialer.Proxy = socket.ConnectionOptions.Proxy
	socket.WebsocketDialer.Subprotocols = socket.ConnectionOptions.Subprotocols
}

// Connect to the server
func (socket *Socket) Connect() {
	var err error
	var resp *http.Response
	socket.setConnectionOptions()

	// Dial the websocket connection
	socket.Conn, resp, err = socket.WebsocketDialer.Dial(socket.Url, socket.RequestHeader)

	if err != nil {
		logger.Error.Println("Error while connecting to server:", err)
		if resp != nil {
			logger.Error.Printf("HTTP Response %d status: %s", resp.StatusCode, resp.Status)
		}
		socket.connStateMu.Lock()
		socket.IsConnected = false
		onConnectError := socket.OnConnectError
		socket.connStateMu.Unlock()

		if onConnectError != nil {
			onConnectError(err, *socket)
		}
		return
	}

	logger.Info.Println("Connected to server")

	socket.connStateMu.Lock()
	socket.IsConnected = true
	onConnected := socket.OnConnected
	socket.connStateMu.Unlock()

	if onConnected != nil {
		onConnected(*socket)
	}

	// Set up handlers
	defaultPingHandler := socket.Conn.PingHandler()
	socket.Conn.SetPingHandler(func(appData string) error {
		logger.Trace.Println("Received PING from server")
		if socket.OnPingReceived != nil {
			socket.OnPingReceived(appData, *socket)
		}
		return defaultPingHandler(appData)
	})

	defaultPongHandler := socket.Conn.PongHandler()
	socket.Conn.SetPongHandler(func(appData string) error {
		logger.Trace.Println("Received PONG from server")
		if socket.OnPongReceived != nil {
			socket.OnPongReceived(appData, *socket)
		}
		return defaultPongHandler(appData)
	})

	defaultCloseHandler := socket.Conn.CloseHandler()
	socket.Conn.SetCloseHandler(func(code int, text string) error {
		result := defaultCloseHandler(code, text)
		logger.Warning.Println("Disconnected from server:", result)
		socket.connStateMu.Lock()
		socket.IsConnected = false
		onDisconnected := socket.OnDisconnected
		socket.connStateMu.Unlock()

		if onDisconnected != nil {
			onDisconnected(errors.New(text), *socket)
		}
		return result
	})

	// Initialize close channel and WaitGroup
	socket.closeChan = make(chan struct{})
	socket.closeWg.Add(1)

	// Start reading messages
	go func() {
		defer socket.closeWg.Done()
		for {
			select {
			case <-socket.closeChan:
				// Received close signal, exiting goroutine
				return
			default:
				socket.receiveMu.Lock()
				if socket.Timeout != 0 {
					socket.Conn.SetReadDeadline(time.Now().Add(socket.Timeout))
				}
				messageType, message, err := socket.Conn.ReadMessage()
				socket.receiveMu.Unlock()
				if err != nil {
					logger.Error.Println("read:", err)
					socket.connStateMu.Lock()
					socket.IsConnected = false
					onDisconnected := socket.OnDisconnected
					socket.connStateMu.Unlock()

					if onDisconnected != nil {
						onDisconnected(err, *socket)
					}
					return
				}
				logger.Info.Printf("recv: %s", message)

				switch messageType {
				case websocket.TextMessage:
					if socket.OnTextMessage != nil {
						socket.OnTextMessage(string(message), *socket)
					}
				case websocket.BinaryMessage:
					if socket.OnBinaryMessage != nil {
						socket.OnBinaryMessage(message, *socket)
					}
				}
			}
		}
	}()
}

// Send a text message
func (socket *Socket) SendText(message string) {
	err := socket.send(websocket.TextMessage, []byte(message))
	if err != nil {
		logger.Error.Println("write:", err)
		return
	}
}

// Send binary data
func (socket *Socket) SendBinary(data []byte) {
	err := socket.send(websocket.BinaryMessage, data)
	if err != nil {
		logger.Error.Println("write:", err)
		return
	}
}

// Internal send function with synchronization
func (socket *Socket) send(messageType int, data []byte) error {
	socket.sendMu.Lock()
	defer socket.sendMu.Unlock()
	err := socket.Conn.WriteMessage(messageType, data)
	return err
}

// Close the connection
func (socket *Socket) Close() {
	// Send close message
	err := socket.send(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	if err != nil {
		logger.Error.Println("write close:", err)
	}

	// Close the websocket connection
	socket.Conn.Close()

	// Signal the goroutine to exit
	close(socket.closeChan)

	// Wait for the goroutine to finish
	socket.closeWg.Wait()

	// Protect access to IsConnected and OnDisconnected
	socket.connStateMu.Lock()
	socket.IsConnected = false
	onDisconnected := socket.OnDisconnected
	socket.connStateMu.Unlock()

	// Call OnDisconnected callback outside of lock
	if onDisconnected != nil {
		onDisconnected(err, *socket)
	}
}
