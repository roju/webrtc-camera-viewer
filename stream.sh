#!/bin/bash

./webrtc-streamer

gst-launch-1.0 -v v4l2src device=/dev/video0 io-mode=4 \
! "video/x-raw,width=1280,height=960" \
! queue \
! mpph264enc profile=baseline header-mode=each-idr \
! rtph264pay \
! udpsink host=127.0.0.1 port=5004