FROM alpine:latest

RUN apk add --no-cache jq wireguard-tools
COPY build/gw.sh .
RUN chmod +x gw.sh

CMD ["./gw.sh"]
