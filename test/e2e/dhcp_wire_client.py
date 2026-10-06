#!/usr/bin/env python3
"""Synthetic DHCPv4 client for the guest-side wire paths of the E2E suite.

Every other case drives the stock cirros udhcpc, which identifies itself by the
MAC in chaddr, never emits option 61 (client identifier) or a foreign option 50
(requested address), and never releases its lease. This client sends exactly
those frames from inside a pod attached to the NAD and decodes the server's
replies from the same raw socket, so the suite can assert what the helper does
with them.

The pod's root filesystem is read-only, so the harness streams this file over
stdin:

    kubectl -n e2e exec -i kih-network-services -c network -- \
        python3 - primary '<spec-json>'

The spec is a JSON array of steps. Every step sends one BOOTP frame and collects
the replies of its own transaction:

    [{"xid": 218103809, "timeout": 6, "settle": 0.5, "expect": "any",
      "send": {"type": "discover", "chaddr": "02:00:00:00:00:51",
               "client_id": "01aabbccddee", "requested_ip": "10.77.0.110",
               "ciaddr": "0.0.0.0", "server_id": "10.77.0.2"}}]

One JSON object per step is written to stdout; the harness asserts on it:

    {"step":0,"xid":218103809,"sent":"discover","expect":"any",
     "replies":[{"message":"OFFER","xid":218103809,"mac":"02:00:00:00:00:51",
                 "ciaddr":"0.0.0.0","yiaddr":"10.77.0.100", ...}],
     "ok":true}

`expect` is "any" (the step must collect at least one reply of its own xid and
chaddr) or "none" (the step must collect none, which is how the unknown-MAC and
release cases prove silence). The client waits for the reply of its own
transaction id only, so no unrelated packet can be mistaken for an answer.

Exit status: 0 when every step met its expectation, 1 when a step did not, 2
when the socket, the interface or the spec is unusable.
"""

import json
import socket
import struct
import sys
import time

ETH_P_ALL = 0x0003
ETH_P_IP = 0x0800
BROADCAST_MAC = b"\xff" * 6
SERVER_PORT = 67
CLIENT_PORT = 68
DHCP_MAGIC = b"\x63\x82\x53\x63"
BROADCAST_FLAG = 0x8000
BOOT_REQUEST = 1
HARDWARE_ETHERNET = 1
HARDWARE_LENGTH = 6
MIN_DHCP_LENGTH = 300
DEFAULT_TIMEOUT = 6.0
DEFAULT_SETTLE = 0.5
MESSAGE_TYPES = {
    1: "DISCOVER",
    2: "OFFER",
    3: "REQUEST",
    4: "DECLINE",
    5: "ACK",
    6: "NAK",
    7: "RELEASE",
    8: "INFORM",
}
SEND_MESSAGE_TYPES = {
    "discover": 1,
    "offer": 2,
    "request": 3,
    "decline": 4,
    "ack": 5,
    "nak": 6,
    "release": 7,
    "inform": 8,
}
OPTION_CLIENT_IDENTIFIER = 61
OPTION_REQUESTED_ADDRESS = 50
OPTION_SERVER_IDENTIFIER = 54
OPTION_HOSTNAME = 12
OPTION_MESSAGE_TYPE = 53


class ClientError(Exception):
    """The client cannot run the requested exchange."""


def parse_mac(text):
    parts = text.split(":")
    if len(parts) != 6:
        raise ClientError(f"invalid MAC {text!r}")
    try:
        return bytes(int(part, 16) for part in parts)
    except ValueError as error:
        raise ClientError(f"invalid MAC {text!r}: {error}") from error


def format_mac(raw):
    return ":".join(f"{octet:02x}" for octet in raw)


def format_ip(raw):
    return ".".join(str(octet) for octet in raw)


def checksum(data):
    if len(data) % 2:
        data += b"\x00"
    total = sum(struct.unpack(f"!{len(data) // 2}H", data))
    while total >> 16:
        total = (total & 0xFFFF) + (total >> 16)
    return (~total) & 0xFFFF


def build_options(send):
    options = []
    if send.get("client_id"):
        options.append((OPTION_CLIENT_IDENTIFIER, bytes.fromhex(send["client_id"])))
    if send.get("requested_ip"):
        options.append((OPTION_REQUESTED_ADDRESS, socket.inet_aton(send["requested_ip"])))
    if send.get("server_id"):
        options.append((OPTION_SERVER_IDENTIFIER, socket.inet_aton(send["server_id"])))
    if send.get("hostname"):
        options.append((OPTION_HOSTNAME, send["hostname"].encode("ascii")))
    return options


def build_bootp(message_type, xid, chaddr, ciaddr, options):
    packet = bytearray()
    packet += struct.pack("!BBBB", BOOT_REQUEST, HARDWARE_ETHERNET, HARDWARE_LENGTH, 0)
    packet += struct.pack("!I", xid)
    packet += struct.pack("!HH", 0, BROADCAST_FLAG)
    packet += socket.inet_aton(ciaddr)
    packet += b"\x00" * 12  # yiaddr, siaddr, giaddr
    packet += chaddr + b"\x00" * (16 - len(chaddr))
    packet += b"\x00" * 64  # sname
    packet += b"\x00" * 128  # file
    packet += DHCP_MAGIC
    packet += bytes([OPTION_MESSAGE_TYPE, 1, message_type])
    for code, value in options:
        packet += bytes([code, len(value)]) + value
    packet += b"\xff"
    if len(packet) < MIN_DHCP_LENGTH:
        packet += b"\x00" * (MIN_DHCP_LENGTH - len(packet))
    return bytes(packet)


def build_frame(chaddr, payload):
    # The synthetic client spoofs its hardware address on the wire as well, so
    # the recorded frame is exactly what a real client with that MAC sends.
    udp = struct.pack("!HHHH", CLIENT_PORT, SERVER_PORT, 8 + len(payload), 0) + payload
    pseudo = (
        socket.inet_aton("0.0.0.0")
        + socket.inet_aton("255.255.255.255")
        + struct.pack("!BBH", 0, socket.IPPROTO_UDP, len(udp))
    )
    udp_checksum = checksum(pseudo + udp) or 0xFFFF
    udp = udp[:6] + struct.pack("!H", udp_checksum) + udp[8:]
    header = struct.pack(
        "!BBHHHBBH", 0x45, 0, 20 + len(udp), 0, 0, 64, socket.IPPROTO_UDP, 0
    )
    header += socket.inet_aton("0.0.0.0") + socket.inet_aton("255.255.255.255")
    header = header[:10] + struct.pack("!H", checksum(header)) + header[12:]
    return BROADCAST_MAC + chaddr + struct.pack("!H", ETH_P_IP) + header + udp


def parse_dhcp(data):
    if len(data) < 240 or data[236:240] != DHCP_MAGIC:
        return None
    if data[1] != HARDWARE_ETHERNET or data[2] != HARDWARE_LENGTH:
        return None
    options = {}
    offset = 240
    while offset < len(data):
        code = data[offset]
        offset += 1
        if code == 0:
            continue
        if code == 255:
            break
        if offset >= len(data):
            return None
        length = data[offset]
        offset += 1
        value = data[offset : offset + length]
        if len(value) != length:
            return None
        options.setdefault(code, value)
        offset += length
    message_value = options.get(OPTION_MESSAGE_TYPE)
    if message_value is None or len(message_value) != 1:
        return None
    message = MESSAGE_TYPES.get(message_value[0])
    if message is None:
        return None
    requested = options.get(OPTION_REQUESTED_ADDRESS)
    client_id = options.get(OPTION_CLIENT_IDENTIFIER)
    server_id = options.get(OPTION_SERVER_IDENTIFIER)
    return {
        "message": message,
        "xid": struct.unpack_from("!I", data, 4)[0],
        "mac": format_mac(data[28:34]),
        "ciaddr": format_ip(data[12:16]),
        "yiaddr": format_ip(data[16:20]),
        "requested_ip": (
            format_ip(requested) if requested is not None and len(requested) == 4 else None
        ),
        "client_id": client_id.hex() if client_id is not None else None,
        "server_id": (
            format_ip(server_id) if server_id is not None and len(server_id) == 4 else None
        ),
    }


def parse_frame(frame):
    if len(frame) < 14:
        return None
    protocol = struct.unpack_from("!H", frame, 12)[0]
    offset = 14
    while protocol in (0x8100, 0x88A8):
        if len(frame) < offset + 4:
            return None
        protocol = struct.unpack_from("!H", frame, offset + 2)[0]
        offset += 4
    if protocol != ETH_P_IP:
        return None
    l2_src = format_mac(frame[6:12])
    l2_dst = format_mac(frame[0:6])
    packet = frame[offset:]
    if len(packet) < 20 or packet[0] >> 4 != 4 or packet[9] != socket.IPPROTO_UDP:
        return None
    header_length = (packet[0] & 0x0F) * 4
    if header_length < 20 or len(packet) < header_length + 8:
        return None
    total_length = struct.unpack_from("!H", packet, 2)[0]
    if total_length < header_length + 8 or total_length > len(packet):
        return None
    packet = packet[:total_length]
    source_ip = format_ip(packet[12:16])
    destination_ip = format_ip(packet[16:20])
    udp = packet[header_length:]
    source_port, destination_port, udp_length = struct.unpack_from("!HHH", udp, 0)
    if {source_port, destination_port} != {SERVER_PORT, CLIENT_PORT}:
        return None
    if udp_length < 8 or udp_length > len(udp):
        return None
    parsed = parse_dhcp(udp[8:udp_length])
    if parsed is None:
        return None
    parsed["src_ip"] = source_ip
    parsed["dst_ip"] = destination_ip
    parsed["l2_src"] = l2_src
    parsed["l2_dst"] = l2_dst
    return parsed


def run_step(sock, interface, index, step):
    send = step["send"]
    send_type = send["type"].lower()
    if send_type not in SEND_MESSAGE_TYPES:
        raise ClientError(f"unknown send type {send['type']!r}")
    xid = step["xid"]
    chaddr = parse_mac(send["chaddr"])
    chaddr_text = format_mac(chaddr)
    payload = build_bootp(
        SEND_MESSAGE_TYPES[send_type],
        xid,
        chaddr,
        send.get("ciaddr", "0.0.0.0"),
        build_options(send),
    )
    sock.sendto(build_frame(chaddr, payload), (interface, 0))

    timeout = float(step.get("timeout", DEFAULT_TIMEOUT))
    settle = float(step.get("settle", DEFAULT_SETTLE))
    deadline = time.monotonic() + timeout
    settle_until = None
    replies = []
    while True:
        now = time.monotonic()
        limit = deadline if settle_until is None else min(deadline, settle_until)
        if now >= limit:
            break
        sock.settimeout(limit - now)
        try:
            frame = sock.recv(65536)
        except (socket.timeout, OSError):
            break
        parsed = parse_frame(frame)
        if parsed is None or parsed["xid"] != xid or parsed["mac"] != chaddr_text:
            continue
        replies.append(parsed)
        if settle_until is None:
            settle_until = time.monotonic() + settle

    expect = step.get("expect", "any")
    if expect == "none":
        ok = not replies
    else:
        ok = bool(replies)
    return {
        "step": index,
        "xid": xid,
        "sent": send_type,
        "chaddr": chaddr_text,
        "expect": expect,
        "replies": replies,
        "ok": ok,
    }


def main(argv):
    if len(argv) != 3:
        print("usage: dhcp_wire_client.py <interface> <spec-json>", file=sys.stderr)
        return 2
    interface, spec_text = argv[1], argv[2]
    try:
        steps = json.loads(spec_text)
    except ValueError as error:
        print(f"dhcp_wire_client: invalid spec: {error}", file=sys.stderr)
        return 2
    if not isinstance(steps, list) or not steps:
        print("dhcp_wire_client: spec must be a non-empty array", file=sys.stderr)
        return 2

    try:
        # The socket carries ETH_P_ALL and the bind keeps protocol 0: binding
        # with an explicit protocol value overrides the receive hook and the
        # socket then receives nothing at all (verified on this kernel), while
        # a zero bind keeps the socket's own ETH_P_ALL hook.
        sock = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(ETH_P_ALL))
        sock.bind((interface, 0))
    except OSError as error:
        print(f"dhcp_wire_client: cannot capture on {interface}: {error}", file=sys.stderr)
        return 2

    failed = 0
    with sock:
        for index, step in enumerate(steps):
            if not isinstance(step, dict):
                print(f"dhcp_wire_client: step {index} is not an object", file=sys.stderr)
                return 2
            try:
                result = run_step(sock, interface, index, step)
            except (ClientError, KeyError, TypeError, ValueError) as error:
                print(f"dhcp_wire_client: step {index}: {error}", file=sys.stderr)
                return 2
            print(json.dumps(result, separators=(",", ":")))
            sys.stdout.flush()
            if not result["ok"]:
                failed = 1
    return failed


if __name__ == "__main__":
    sys.exit(main(sys.argv))
