#!/usr/bin/env python3
"""Rasterize the simple geometric mark from static/favicon.svg, without dependencies.

Keep the X (10,12)-(22,28), blue dot (30,27), and 40-unit viewbox in sync
with that SVG. Four samples per axis keep the PNG edges smooth. Maskable
artwork is scaled into the central 80% so OS icon masks cannot crop it.
"""
from pathlib import Path
import struct
import zlib

OUT = Path(__file__).resolve().parents[1] / 'internal/web/static'


def segment_distance(x, y, ax, ay, bx, by):
    t = max(0, min(1, ((x-ax)*(bx-ax)+(y-ay)*(by-ay))/((bx-ax)**2+(by-ay)**2)))
    return (x-ax-t*(bx-ax))**2 + (y-ay-t*(by-ay))**2


def pixel(x, y, maskable):
    if maskable:
        x, y = (x-20)/.8+20, (y-20)/.8+20
    if (x-30)**2+(y-27)**2 <= 9:
        return (29, 155, 240)
    if min(segment_distance(x,y,10,12,22,28), segment_distance(x,y,22,12,10,28)) <= 2.25:
        return (255, 255, 255)
    return (15, 20, 25)


def chunk(kind, data):
    return struct.pack('>I',len(data))+kind+data+struct.pack('>I',zlib.crc32(kind+data))


def generate(name, size, maskable=False):
    rows = bytearray()
    for y in range(size):
        rows.append(0)
        for x in range(size):
            samples = [pixel((x+(sx+.5)/4)*40/size, (y+(sy+.5)/4)*40/size, maskable)
                       for sy in range(4) for sx in range(4)]
            rows.extend(round(sum(c[i] for c in samples)/16) for i in range(3))
    png = b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB',size,size,8,2,0,0,0))
    png += chunk(b'IDAT',zlib.compress(bytes(rows),9))+chunk(b'IEND',b'')
    (OUT/name).write_bytes(png)


for filename, size, maskable in [('icon-192.png',192,False),('icon-512.png',512,False),
                                 ('icon-maskable-512.png',512,True),('apple-touch-icon.png',180,False)]:
    generate(filename,size,maskable)
