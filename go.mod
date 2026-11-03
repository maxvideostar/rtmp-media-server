module github.com/maxvideostar/rtmp-media-server

go 1.24

replace golang.org/x/sys => github.com/golang/sys v0.34.0

require (
	github.com/datarhei/joy4 v0.0.0-20260529103716-aae50349bd84
	github.com/kardianos/service v1.3.0
)

require golang.org/x/sys v0.34.0 // indirect
