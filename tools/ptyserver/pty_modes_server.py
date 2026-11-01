# Captures the PTY terminal modes our sidecar requests in its pty-req and
# writes them to pty-modes.json next to this script, then echoes channel
# input like tools/testsrv. This lets the driver script assert exactly what
# the server's line discipline would receive (RFC 4254 §8): on Linux the
# kernel tty applies these flags, so a stray IGNCR discards every CR the
# xterm front end sends for Enter.
# Prints "READY <port>" on stdout, serves one session, exits on disconnect.
import json
import os
import socket
import sys
import threading

import paramiko

OUT_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "pty-modes.json")
MODE_ECHO = 53  # RFC 4254 pty-mode opcode for ECHO
MODE_IGNCR = 35  # opcode for IGNCR (ignore CR on input)


def parse_modes(raw):
    # paramiko <5 hands over a parsed {opcode: value} dict; paramiko 5 passes
    # the raw wire blob: (opcode: byte, value: u32 big-endian) pairs, ended by
    # opcode 0 (RFC 4254 §8).
    if isinstance(raw, dict):
        return {str(op): value for op, value in raw.items()}
    data = bytes(raw)
    modes = {}
    offset = 0
    while offset + 5 <= len(data):
        opcode = data[offset]
        if opcode == 0:
            break
        modes[str(opcode)] = int.from_bytes(data[offset + 1 : offset + 5], "big")
        offset += 5
    return modes


class Server(paramiko.ServerInterface):
    def __init__(self):
        self.event = threading.Event()

    def check_auth_password(self, username, password):
        if username == "tester" and password == "pw123":
            return paramiko.AUTH_SUCCESSFUL
        return paramiko.AUTH_FAILED

    def get_allowed_auths(self, username):
        return "password"

    def check_channel_pty_request(self, channel, term, width, height, pixelwidth, pixelheight, modes):
        dump = parse_modes(modes)
        dump = {"parsed": dump, "rawHex": "" if isinstance(modes, dict) else bytes(modes).hex()}
        with open(OUT_PATH, "w", encoding="utf-8") as handle:
            json.dump(dump, handle, indent=2)
        print(f"PTY-REQ term={term} {width}x{height} modes={dump}", flush=True)
        return True

    def check_channel_shell_request(self, channel):
        self.event.set()
        return True

    def check_channel_request(self, kind, chanid):
        return paramiko.OPEN_SUCCEEDED


def echo_session(channel):
    try:
        channel.send("TEST-PTY-BANNER\r\n")
        while True:
            data = channel.recv(4096)
            if not data:
                break
            channel.send(b"ECHO:" + data)
    except Exception:
        pass
    finally:
        try:
            channel.close()
        except Exception:
            pass


def main():
    listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(("127.0.0.1", 0))
    listener.listen(1)
    port = listener.getsockname()[1]
    host_key = paramiko.RSAKey.generate(2048)
    print(f"READY {port}", flush=True)

    sock, _ = listener.accept()
    transport = paramiko.Transport(sock)
    transport.add_server_key(host_key)
    server = Server()
    transport.start_server(server=server)

    channel = transport.accept(30)
    if channel is None:
        print("no channel", flush=True)
        sys.exit(1)
    if not server.event.wait(15):
        print("shell request never arrived", flush=True)
        sys.exit(1)
    echo_session(channel)
    transport.close()
    print("DONE", flush=True)


if __name__ == "__main__":
    main()
