FROM alpine:latest

RUN apk add --no-cache jq wireguard-tools
COPY build/gw.sh .

ENTRYPOINT ["./gw.sh"]
