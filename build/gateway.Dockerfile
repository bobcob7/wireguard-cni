FROM alpine:latest

RUN apk add --no-cache jq wireguard-tools
COPY build/gw.sh .
RUN chmod +x gw.sh

ENTRYPOINT ["./gw.sh"]
