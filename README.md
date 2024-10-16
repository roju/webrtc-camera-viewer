# webrtc-camera-viewer

Show a live view of a v4l2 camera remotely with low latency using WebRTC.

## Basic overview

Gstreamer sets the video properties, encodes to H.264 using the hardware encoder, and sends RTP packets via UDP to localhost.

Pion WebRTC consumes the RTP stream and sends it to a WebRTC client.

The webpage runs the WebRTC client and shows the live view of the camera.

## Tested platform

Hardware: Radxa Zero 3W (RK3566)\
System Image: Debian Bullseye (officially supported by Radxa)\
Camera: Radxa Camera 8M 219

## Usage

1. Build the WebRTC streamer binary

    ```sh
    go mod init webrtc-streamer
    go mod tidy
    go build
    ```

2. Start the streaming server

    ```sh
    ./stream.sh
    ```

3. On a separate device connected to the same network, open the webpage at `http://<Board IP Address>:8080/`
4. Click the "View Camera" button
