let pc = new RTCPeerConnection({
  iceServers: [
    {
      urls: 'stun:stun.l.google.com:19302'
    }
  ]
})
let log = msg => {
  document.getElementById('logs').innerHTML += msg + '<br>'
}

pc.ontrack = function (event) {
  var el = document.createElement(event.track.kind)
  el.srcObject = event.streams[0]
  el.autoplay = true
  el.controls = true

  document.getElementById('remoteVideo').appendChild(el)
}

pc.oniceconnectionstatechange = e => log(pc.iceConnectionState)

// Offer to receive 1 video track
pc.addTransceiver('video', {'direction': 'recvonly'})
pc.createOffer().then(d => pc.setLocalDescription(d)).catch(log)


document.getElementById('viewCamera').addEventListener('click', function() {
  fetch('/post', {
      method: 'POST',
      body: btoa(JSON.stringify(pc.localDescription))
  })
  .then(response => response.text())
  .then(data => {
    console.log('recv sd from cam')
    console.log(data)
    pc.setRemoteDescription(new RTCSessionDescription(JSON.parse(atob(data))))
  })
  .catch(error => console.error('Error:', error));
});