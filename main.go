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

		sessionContext, cancelSession := context.WithCancel(context.Background())
		peerConnection, videoTrack, rtpSender := initWebRTCSession(&offer)
		go readIncomingRTCPPackets(rtpSender, sessionContext)
		listener := initUDPListener()
		go sendRtpToClient(videoTrack, listener, sessionContext)
		gstHandle := runGstreamerPipeline(gstContext)
		handleICEConnectionState(peerConnection, cancelSession, gstHandle, listener)

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
	cancelSession context.CancelFunc,
	gstHandle *exec.Cmd,
	listener *net.UDPConn,
) {
	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateClosed {
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
				panic(err)
			}
			fmt.Println("UDP listener closed")
			cancelSession()
			fmt.Println("cancelSession called")
		}
	})
}

// Before these packets are returned they are processed by interceptors. For things
// like NACK this needs to be called.
func readIncomingRTCPPackets(rtpSender *webrtc.RTPSender, sessionContext context.Context) {
	rtcpBuf := make([]byte, 1500)
	for {
		select {
		case <-sessionContext.Done():
			fmt.Println("readIncomingRTCPPackets recv sessionContext.Done()")
			return
		default:
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				fmt.Println("readIncomingRTCPPackets rtpSender.Read error")
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

func sendRtpToClient(videoTrack *webrtc.TrackLocalStaticRTP, listener *net.UDPConn, sessionContext context.Context) {
	// Read RTP packets and send them to the WebRTC Client
	inboundRTPPacket := make([]byte, 1600) // UDP MTU
	for {
		select {
		case <-sessionContext.Done():
			fmt.Println("sendRtpToClient recv sessionContext.Done()")
			return
		default:
			fmt.Println("sendRtpToClient about to readFrom")
			n, _, err := listener.ReadFrom(inboundRTPPacket)
			if err != nil {
				fmt.Println("sendRtpToClient ReadFrom() error")
				panic(fmt.Sprintf("error during read: %s", err))
			}
			fmt.Println("sendRtpToClient about to write")
			if _, err = videoTrack.Write(inboundRTPPacket[:n]); err != nil {
				if errors.Is(err, io.ErrClosedPipe) {
					// The peerConnection has been closed.
					fmt.Println("UDP listener ErrClosedPipe")
					return
				}
				fmt.Println("sendRtpToClient videoTrack.Write() error")
				panic(err)
			}
			fmt.Println("inboundRTPPacket written to videoTrack")
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
