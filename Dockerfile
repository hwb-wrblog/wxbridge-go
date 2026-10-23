FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /wxbridge ./cmd/wechat-flow

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /wxbridge /usr/local/bin/wxbridge
VOLUME /data
EXPOSE 8787
ENTRYPOINT ["wxbridge", "-state", "/data", "serve", "-listen", "0.0.0.0:8787"]
