package main

import (
	"fmt"
	"github.com/ninjahome/webrtc/demo/internal"
	"github.com/pion/mediadevices"
	"github.com/pion/mediadevices/pkg/codec/opus"
	"github.com/pion/mediadevices/pkg/codec/vpx"
	"github.com/pion/mediadevices/pkg/codec/x264"
	_ "github.com/pion/mediadevices/pkg/driver/camera"
	_ "github.com/pion/mediadevices/pkg/driver/microphone"
	"github.com/pion/mediadevices/pkg/frame"
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/ivfwriter"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
	"os"
	"strings"
)

func main() {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	x264Params, err := x264.NewParams()
	internal.Must(err)

	vp8Params, err := vpx.NewVP8Params()
	internal.Must(err)

	opusParams, err := opus.NewParams()
	internal.Must(err)
	codecSelector := mediadevices.NewCodecSelector(
		mediadevices.WithVideoEncoders(&x264Params),
		mediadevices.WithVideoEncoders(&vp8Params),
		mediadevices.WithAudioEncoders(&opusParams),
	)
	mediaEngine := &webrtc.MediaEngine{}

	codecSelector.Populate(mediaEngine)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	var peerConnection, peerErr = api.NewPeerConnection(config)
	internal.Must(peerErr)
	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("cannot close peerConnection: %v\n", cErr)
		}
	}()

	mediaStream, err := mediadevices.GetUserMedia(mediadevices.MediaStreamConstraints{
		Video: func(c *mediadevices.MediaTrackConstraints) {
			c.FrameFormat = prop.FrameFormat(frame.FormatI420)
			c.Width = prop.Int(640)
			c.Height = prop.Int(480)
		},
		Audio: func(c *mediadevices.MediaTrackConstraints) {
		},
		Codec: codecSelector,
	})
	internal.Must(err)

	for _, track := range mediaStream.GetTracks() {
		track.OnEnded(func(err error) {
			fmt.Printf("Track (ID: %s) ended with error: %v\n",
				track.ID(), err)
		})

		_, err := peerConnection.AddTransceiverFromTrack(track,
			webrtc.RTPTransceiverInit{
				Direction: webrtc.RTPTransceiverDirectionSendrecv,
			},
		)
		internal.Must(err)
	}

	_, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo)
	internal.Must(err)
	_, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio)
	internal.Must(err)

	var oggFile, oggErr = oggwriter.New("output.ogg", 48000, 2)
	internal.Must(oggErr)
	var ivfFile, ivfErr = ivfwriter.New("output_answer.ivf")
	internal.Must(ivfErr)

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		codec := track.Codec()
		fmt.Println("------>>>codec:", codec)
		if strings.EqualFold(codec.MimeType, webrtc.MimeTypeOpus) {
			internal.SaveToDisk(oggFile, track)
		} else if strings.EqualFold(codec.MimeType, webrtc.MimeTypeH264) {
			internal.SaveToDisk(ivfFile, track)
		} else if strings.EqualFold(codec.MimeType, webrtc.MimeTypeVP8) {
			internal.SaveToDisk(ivfFile, track)
		} else if strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeAV1) {
			internal.SaveToDisk(ivfFile, track)
		}
	})

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateConnected {
			fmt.Println("Ctrl+C the remote client to stop the demo")
		} else if connectionState == webrtc.ICEConnectionStateFailed {
			//if closeErr := oggFile.Close(); closeErr != nil {
			//	panic(closeErr)
			//}
			//
			//if closeErr := ivfFile.Close(); closeErr != nil {
			//	panic(closeErr)
			//}

			fmt.Println("Done writing media files")

			// Gracefully shutdown the peer connection
			if closeErr := peerConnection.Close(); closeErr != nil {
				panic(closeErr)
			}

			os.Exit(0)
		}
	})
	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("Peer Connection State has changed: %s\n", s.String())
		if s == webrtc.PeerConnectionStateFailed {
			fmt.Println("Peer Connection has gone to failed exiting")
			os.Exit(0)
		}
	})

	offer := webrtc.SessionDescription{}
	internal.Decode(internal.MustReadStdin(), &offer)

	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		panic(err)
	}

	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	<-gatherComplete

	fmt.Println(internal.Encode(*peerConnection.LocalDescription()))

	select {}
}
