#!/usr/bin/env python3
"""BEP-15 connect handshake against a UDP tracker."""
import os, socket, struct, sys

host, port = sys.argv[1], int(sys.argv[2])
tid = struct.unpack(">I", os.urandom(4))[0]
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(5)
s.sendto(struct.pack(">QII", 0x41727101980, 0, tid), (host, port))
data, _ = s.recvfrom(64)
action, rtid, _conn = struct.unpack(">IIQ", data[:16])
assert action == 0 and rtid == tid, (action, rtid)
print("udp tracker ok")
