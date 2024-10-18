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

	"github.com/pion/webrtc/v4"
)

func main() {
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)

	endStream := make(chan bool)
	gstContext, cancelGst := context.WithCancel(context.Background())
	defer cancelGst()

	http.HandleFunc("/post", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
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

		localSD, videoTrack, rtpSender := initWebRTCSession(&offer, endStream, cancelGst)
		go readIncomingRTCPPackets(rtpSender, endStream)
		go sendRtpToClient(videoTrack, endStream)
		runGstreamerPipeline(gstContext)

		fmt.Fprint(w, encode(localSD))
		fmt.Println("Sent local SessionDescription to browser")
	})

	fmt.Println("Server starting on :8080...")
	err := http.ListenAndServe(":8080", nil)
	log.Fatal("HTTP Server error: ", err)
}

func initWebRTCSession(
	offer *webrtc.SessionDescription,
	endStream chan bool,
	cancelGst context.CancelFunc,
) (
	*webrtc.SessionDescription,
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

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateFailed {
			endStream <- true
			cancelGst()
			if closeErr := peerConnection.Close(); closeErr != nil {
				panic(closeErr)
			}
			fmt.Println("peerConnection closed")
		}
	})

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

	return peerConnection.LocalDescription(), videoTrack, rtpSender
}

// Before these packets are returned they are processed by interceptors. For things
// like NACK this needs to be called.
func readIncomingRTCPPackets(rtpSender *webrtc.RTPSender, endStream chan bool) {
	rtcpBuf := make([]byte, 1500)
	for {
		select {
		case <-endStream:
			return
		default:
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
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

func sendRtpToClient(videoTrack *webrtc.TrackLocalStaticRTP, endStream chan bool) {
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

	defer func() {
		if err = listener.Close(); err != nil {
			panic(err)
		}
	}()

	// Read RTP packets forever and send them to the WebRTC Client
	inboundRTPPacket := make([]byte, 1600) // UDP MTU
	for {
		select {
		case <-endStream:
			return
		default:
			n, _, err := listener.ReadFrom(inboundRTPPacket)
			if err != nil {
				panic(fmt.Sprintf("error during read: %s", err))
			}

			if _, err = videoTrack.Write(inboundRTPPacket[:n]); err != nil {
				if errors.Is(err, io.ErrClosedPipe) {
					// The peerConnection has been closed.
					return
				}
				panic(err)
			}
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
