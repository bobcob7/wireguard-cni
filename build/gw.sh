#!/bin/sh
cat "Forwarding" > logs.txt
trap : TERM INT

# Get interface
INT=$(ip -j link show type wireguard | jq -r '.[0].ifname')
echo "WG Interface $INT"

# Get WG Endpoint
END=$(wg show $INT endpoints | awk -F '[\t /:]' '{print $3}')
echo "Endpoint $END"

# Route all traffic over to the tunnel
ip route add 0.0.0.0/1 dev $INT
ip route add 128.0.0.0/1 dev $INT
ip route add $END/32 dev eth0

# Setup NAT
iptables -A FORWARD -i net1 -o $INT -m state --state ESTABLISHED,RELATED -j ACCEPT
iptables -t nat -A POSTROUTING -o $INT -j MASQUERADE

sleep infinity & wait
