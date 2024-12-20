package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strings"

	"github.com/pion/webrtc/v4"
)

func main() {
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)

	gstContext, cancelGst := context.WithCancel(context.Background())
	defer cancelGst()
	var streamInProgress bool = false

	http.HandleFunc("/post", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if streamInProgress {
			fmt.Println("Attempted new session while stream in progress")
			http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Unable to read request", http.StatusInternalServerError)
			return
		}
		fmt.Println("Received SessionDescription from browser")

		defer r.Body.Close()
		offer := webrtc.SessionDescription{}
		decode(string(body), &offer)

		peerConnection, videoTrack, rtpSender := initWebRTCSession(&offer)
		go readIncomingRTCPPackets(rtpSender)
		listener := initUDPListener()
		go sendRtpToClient(videoTrack, listener)
		gstHandle := runGstreamerPipeline(gstContext)
		handleICEConnectionState(peerConnection, gstHandle, listener, &streamInProgress)
		streamInProgress = true

		fmt.Fprint(w, encode(peerConnection.LocalDescription()))
		fmt.Println("Sent local SessionDescription to browser")
	})

	fmt.Println("Server starting on :8080...")
	err := http.ListenAndServe(":8080", nil)
	log.Fatal("HTTP Server error: ", err)
}

func initWebRTCSession(offer *webrtc.SessionDescription) (
	*webrtc.PeerConnection,
	*webrtc.TrackLocalStaticRTP,
	*webrtc.RTPSender,
) {
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	})
	if err != nil {
		panic(err)
	}

	// Create a video track
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "pion")
	if err != nil {
		panic(err)
	}
	rtpSender, err := peerConnection.AddTrack(videoTrack)
	if err != nil {
		panic(err)
	}

	// Set the remote SessionDescription
	if err = peerConnection.SetRemoteDescription(*offer); err != nil {
		panic(err)
	}

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	fmt.Println("WebRTC session initialized")
	return peerConnection, videoTrack, rtpSender
}

func handleICEConnectionState(
	peerConnection *webrtc.PeerConnection,
	gstHandle *exec.Cmd,
	listener *net.UDPConn,
	streamInProgress *bool,
) {
	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateClosed ||
			connectionState == webrtc.ICEConnectionStateDisconnected ||
			connectionState == webrtc.ICEConnectionStateFailed {
			if !*streamInProgress {
				return
			}
			if err := gstHandle.Process.Kill(); err != nil {
				fmt.Println("Failed to terminate Gstreamer: ", err)
			} else {
				fmt.Println("Terminated Gstreamer")
			}
			if err := peerConnection.Close(); err != nil {
				panic(err)
			}
			fmt.Println("peerConnection closed")

			if err := listener.Close(); err != nil {
				if !strings.Contains(err.Error(), "use of closed network connection") {
					fmt.Println("OnICEConnectionStateChange listener.Close() error:", err)
				}
			} else {
				fmt.Println("UDP listener closed")
			}
			*streamInProgress = false
		}
	})
}

// Before these packets are returned they are processed by interceptors. For things
// like NACK this needs to be called.
func readIncomingRTCPPackets(rtpSender *webrtc.RTPSender) {
	rtcpBuf := make([]byte, 1500)
	for {
		if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
			// fmt.Println("rtpSender.Read error", rtcpErr)
			return
		}
	}
}

func runGstreamerPipeline(ctx context.Context) *exec.Cmd {
	const (
		videoWidth  = 1280
		videoHeight = 960
		rtpPort     = 5004
	)

	args := []string{
		"v4l2src", "device=/dev/video0", "io-mode=4",
		"!", fmt.Sprintf("video/x-raw,width=%d,height=%d", videoWidth, videoHeight),
		"!", "queue",
		"!", "mpph264enc", "profile=baseline", "header-mode=each-idr",
		"!", "rtph264pay",
		"!", "udpsink", "host=127.0.0.1",
		fmt.Sprintf("port=%d", rtpPort),
	}

	cmd := exec.CommandContext(ctx, "gst-launch-1.0", args...)

	// stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	fmt.Println("Gstreamer running...")

	// scanner := bufio.NewScanner(stderr)
	// scanner.Split(bufio.ScanWords)
	// for scanner.Scan() {
	// 	m := scanner.Text()
	// 	fmt.Println(m)
	// }
	// cmd.Wait()

	return cmd
}

func initUDPListener() *net.UDPConn {
	// Open a UDP Listener for RTP Packets on port 5004
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5004})
	if err != nil {
		panic(err)
	}

	// Increase the UDP receive buffer size
	// Default UDP buffer sizes vary on different operating systems
	bufferSize := 300000 // 300KB
	err = listener.SetReadBuffer(bufferSize)
	if err != nil {
		panic(err)
	}

	return listener
}

func sendRtpToClient(videoTrack *webrtc.TrackLocalStaticRTP, listener *net.UDPConn) {
	defer func() {
		if err := listener.Close(); err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				fmt.Println("sendRtpToClient listener.Close() error:", err)
			}
		} else {
			fmt.Println("UDP listener closed")
		}
	}()

	// Read RTP packets and send them to the WebRTC Client
	inboundRTPPacket := make([]byte, 1600) // UDP MTU
	for {
		n, _, err := listener.ReadFrom(inboundRTPPacket)
		if err != nil {
			// fmt.Println("UDPConn error during read:", err)
			return
		}
		if _, err = videoTrack.Write(inboundRTPPacket[:n]); err != nil {
			if errors.Is(err, io.ErrClosedPipe) {
				fmt.Println("TrackLocalStaticRTP ErrClosedPipe")
				return
			}
			fmt.Println("sendRtpToClient videoTrack.Write() error")
			panic(err)
		}
	}
}

// JSON encode + base64 a SessionDescription
func encode(obj *webrtc.SessionDescription) string {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}

	return base64.StdEncoding.EncodeToString(b)
}

// Decode a base64 and unmarshal JSON into a SessionDescription
func decode(in string, obj *webrtc.SessionDescription) {
	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		panic(err)
	}

	if err = json.Unmarshal(b, obj); err != nil {
		panic(err)
	}
}
