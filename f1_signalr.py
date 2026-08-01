#!/usr/bin/env python3
"""
Standalone F1 Live Timing SignalR client — a Python port of apps/backend/src/services/f1-client.ts.

Connects to Formula 1's official ASP.NET Core SignalR live-timing endpoint and prints
every event to stdout. No token or account is required: the connectionToken is obtained
automatically from the public /negotiate endpoint.

Note: F1 only broadcasts data during live sessions (practice/quali/race). Outside a session
you will connect successfully but see only periodic Heartbeat frames (or nothing).

Requires: pip install websockets
"""

import asyncio
import base64
import json
import urllib.request
import urllib.parse
import zlib
from datetime import datetime, timezone

import websockets

# --- Constants (mirrored from core/src/constants.ts and f1-client.ts) ---

F1_SERVER_URL = "https://livetiming.formula1.com/signalrcore"
F1_ORIGIN_URL = "https://www.formula1.com"
NEGOTIATE_VERSION = "1"

# SignalR Core framing: every JSON record on the wire ends with the 0x1E record separator
RECORD_SEPARATOR = "\x1e"
FEED_TARGET = "feed"
SUBSCRIBE_INVOCATION_ID = "1"
MESSAGE_TYPE_INVOCATION = 1
MESSAGE_TYPE_COMPLETION = 3
MESSAGE_TYPE_PING = 6
MESSAGE_TYPE_CLOSE = 7

# Server drops idle clients after ~30s — periodic client pings keep the connection alive
PING_INTERVAL_S = 15

# Reconnect backoff
BASE_RECONNECT_DELAY_S = 5
MAX_RECONNECT_DELAY_S = 60

SESSION_INFO_CHANNEL = "SessionInfo"

# All available F1 Live Timing channels
SUBSCRIBE_CHANNELS = [
    "CarData.z",
    "Position.z",
    "TimingData",
    "TimingDataF1",
    "TimingAppData",
    "TimingStats",
    "TrackStatus",
    "SessionInfo",
    "DriverList",
    "WeatherData",
    "RaceControlMessages",
    "ExtrapolatedClock",
    "LapCount",
    "SessionData",
    "Heartbeat",
]

# Pre-serialised protocol messages (built once, sent on every (re)connect)
HANDSHAKE_MESSAGE = json.dumps({"protocol": "json", "version": 1}) + RECORD_SEPARATOR
PING_MESSAGE = json.dumps({"type": MESSAGE_TYPE_PING}) + RECORD_SEPARATOR
SUBSCRIBE_MESSAGE = (
    json.dumps(
        {
            "arguments": [SUBSCRIBE_CHANNELS],
            "invocationId": SUBSCRIBE_INVOCATION_ID,
            "target": "Subscribe",
            "type": MESSAGE_TYPE_INVOCATION,
        }
    )
    + RECORD_SEPARATOR
)

NEGOTIATE_HEADERS = {
    "User-Agent": "BestHTTP",
    "Accept-Encoding": "gzip, deflate",
    "Origin": F1_ORIGIN_URL,
    "Referer": f"{F1_ORIGIN_URL}/",
}


def log(msg: str) -> None:
    ts = datetime.now(timezone.utc).strftime("%H:%M:%S")
    print(f"[{ts}] {msg}", flush=True)


def decompress_payload(base64_payload: str):
    """F1 compresses high-frequency channels (.z) with raw DEFLATE (no zlib wrapper)."""
    try:
        raw = zlib.decompress(base64.b64decode(base64_payload), -zlib.MAX_WBITS)
        return json.loads(raw.decode("utf-8"))
    except Exception as err:  # noqa: BLE001 — surface then keep going
        log(f"Failed to decompress payload: {err}")
        return None


def negotiate():
    """SignalR Core negotiate is a POST; returns (connectionToken, sessionCookie)."""
    url = f"{F1_SERVER_URL}/negotiate?negotiateVersion={NEGOTIATE_VERSION}"
    req = urllib.request.Request(url, data=b"", method="POST", headers=NEGOTIATE_HEADERS)
    with urllib.request.urlopen(req, timeout=15) as resp:
        set_cookie = resp.headers.get("set-cookie", "")
        session_cookie = set_cookie.split(";")[0] if set_cookie else ""
        data = json.loads(resp.read().decode("utf-8"))

    token = data.get("connectionToken")
    if not token:
        raise RuntimeError("Missing connectionToken in negotiate response")
    return urllib.parse.quote(token), session_cookie


def handle_frame(frame: dict) -> None:
    """Route a single SignalR Core record by message type."""
    ftype = frame.get("type")

    # Live updates arrive as 'feed' invocations: arguments = [channel, payload, timestamp]
    if ftype == MESSAGE_TYPE_INVOCATION and frame.get("target") == FEED_TARGET:
        args = frame.get("arguments") or []
        if len(args) >= 2:
            channel, payload = args[0], args[1]
            if channel.endswith(".z") and isinstance(payload, str):
                payload = decompress_payload(payload)
            log(f"EVENT  {channel}: {json.dumps(payload)[:500]}")
        return

    # Subscribe completion carries the full state snapshot in `result`
    if ftype == MESSAGE_TYPE_COMPLETION:
        if frame.get("error"):
            log(f"Subscribe invocation failed: {frame['error']}")
            return
        result = frame.get("result")
        if isinstance(result, dict):
            log(f"SNAPSHOT  {len(result)} channels")
            for channel, raw in result.items():
                if channel.endswith(".z") and isinstance(raw, str):
                    raw = decompress_payload(raw)
                log(f"  {channel}: {json.dumps(raw)[:300]}")
        return

    if ftype == MESSAGE_TYPE_CLOSE:
        log("F1 server requested connection close.")


async def ping_loop(ws) -> None:
    """Keep the SignalR Core connection alive — the server times out silent clients."""
    while True:
        await asyncio.sleep(PING_INTERVAL_S)
        await ws.send(PING_MESSAGE)


async def run_once() -> None:
    log(f"Negotiating with F1 SignalR at {F1_SERVER_URL}...")
    token, session_cookie = negotiate()

    ws_base = F1_SERVER_URL.replace("http", "ws")
    connect_url = f"{ws_base}?id={token}"

    headers = {"User-Agent": "BestHTTP", "Origin": F1_ORIGIN_URL}
    if session_cookie:
        headers["Cookie"] = session_cookie

    # `additional_headers` for websockets>=14; older versions use `extra_headers`.
    try:
        conn = websockets.connect(connect_url, additional_headers=headers)
    except TypeError:
        conn = websockets.connect(connect_url, extra_headers=headers)

    async with conn as ws:
        log("Connected to F1 SignalR Core WebSocket.")
        # SignalR Core requires the JSON protocol handshake before any hub invocation
        await ws.send(HANDSHAKE_MESSAGE)
        await ws.send(SUBSCRIBE_MESSAGE)
        log(f"Subscribed to: {', '.join(SUBSCRIBE_CHANNELS)}")

        pinger = asyncio.create_task(ping_loop(ws))
        try:
            async for message in ws:
                if isinstance(message, bytes):
                    message = message.decode("utf-8")
                # A single WS message may carry multiple 0x1E-terminated JSON records
                for record in message.split(RECORD_SEPARATOR):
                    if len(record) < 3:  # skip empty trailing segment and '{}' handshake ack
                        continue
                    try:
                        handle_frame(json.loads(record))
                    except json.JSONDecodeError as err:
                        log(f"Error processing SignalR message: {err}")
        finally:
            pinger.cancel()


async def main() -> None:
    """Reconnect loop with exponential backoff, mirroring the backend client."""
    attempts = 0
    while True:
        try:
            await run_once()
            attempts = 0  # clean exit resets backoff
        except Exception as err:  # noqa: BLE001 — reconnect on any failure
            log(f"Connection error: {err}")

        delay = min(BASE_RECONNECT_DELAY_S * (2 ** attempts), MAX_RECONNECT_DELAY_S)
        attempts += 1
        log(f"Reconnecting in {delay}s (attempt {attempts})...")
        await asyncio.sleep(delay)


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        log("Stopped.")
