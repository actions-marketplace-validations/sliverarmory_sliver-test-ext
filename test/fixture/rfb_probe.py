#!/usr/bin/env python3
"""Verify the stateful fixture's RFB 3.8 handshake and raw framebuffer."""

import argparse
import socket
import struct


BANNER = b"RFB 003.008\n"
DESKTOP_NAME = b"sliver-ext-e2e"
PIXELS = bytes(
    [
        0, 0, 255, 0,  # red
        0, 255, 0, 0,  # green
        255, 0, 0, 0,  # blue
        255, 255, 255, 0,  # white
    ]
)


def receive_exact(connection: socket.socket, length: int) -> bytes:
    result = bytearray()
    while len(result) < length:
        chunk = connection.recv(length - len(result))
        if not chunk:
            raise AssertionError(f"RFB peer closed after {len(result)} of {length} bytes")
        result.extend(chunk)
    return bytes(result)


def verify(port: int) -> None:
    with socket.create_connection(("127.0.0.1", port), timeout=3) as connection:
        connection.settimeout(3)
        assert receive_exact(connection, 12) == BANNER, "unexpected RFB version banner"
        connection.sendall(BANNER)

        assert receive_exact(connection, 2) == b"\x01\x01", "unexpected security types"
        connection.sendall(b"\x01")  # None authentication.
        assert receive_exact(connection, 4) == b"\x00\x00\x00\x00", "security failed"
        connection.sendall(b"\x01")  # Shared desktop.

        width, height = struct.unpack(">HH", receive_exact(connection, 4))
        assert (width, height) == (2, 2), f"unexpected framebuffer size: {width}x{height}"
        pixel_format = receive_exact(connection, 16)
        assert pixel_format == bytes(
            [32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0]
        ), "unexpected pixel format"
        (name_length,) = struct.unpack(">I", receive_exact(connection, 4))
        assert name_length == len(DESKTOP_NAME), "unexpected desktop name length"
        assert receive_exact(connection, name_length) == DESKTOP_NAME, "unexpected desktop name"

        connection.sendall(b"\x03\x00\x00\x00\x00\x00\x00\x02\x00\x02")
        assert receive_exact(connection, 4) == b"\x00\x00\x00\x01", "unexpected update header"
        assert receive_exact(connection, 12) == bytes(
            [0, 0, 0, 0, 0, 2, 0, 2, 0, 0, 0, 0]
        ), "unexpected rectangle"
        assert receive_exact(connection, len(PIXELS)) == PIXELS, "unexpected framebuffer pixels"


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, required=True)
    args = parser.parse_args()
    if not 1 <= args.port <= 65535:
        parser.error("--port must be between 1 and 65535")
    verify(args.port)
    print("rfb-3.8-handshake-and-framebuffer-ok")


if __name__ == "__main__":
    main()
