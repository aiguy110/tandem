import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import type { Db } from './db.ts';

export const MAX_ASSET_BYTES = 10 * 1024 * 1024;
export const MAX_PROMPT_IMAGES = 4;
export const MAX_PROMPT_IMAGE_BYTES = 20 * 1024 * 1024;
export const MAX_IMAGE_PIXELS = 40_000_000;
export const MAX_IMAGE_DIMENSION = 16_384;

export interface StoredAsset {
  assetId: string;
  mimeType: string;
  size: number;
}

export class UnsupportedAssetError extends Error {}
export class AssetTooLargeError extends Error {}

function sniffMime(data: Buffer): string | undefined {
  if (data.length >= 8 && data.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]))) return 'image/png';
  if (data.length >= 3 && data[0] === 0xff && data[1] === 0xd8 && data[2] === 0xff) return 'image/jpeg';
  if (data.length >= 6 && (data.subarray(0, 6).toString('ascii') === 'GIF87a' || data.subarray(0, 6).toString('ascii') === 'GIF89a')) return 'image/gif';
  if (data.length >= 12 && data.subarray(0, 4).toString('ascii') === 'RIFF' && data.subarray(8, 12).toString('ascii') === 'WEBP') return 'image/webp';
  return undefined;
}

function imageDimensions(data: Buffer, mimeType: string): { width: number; height: number } | undefined {
  if (mimeType === 'image/png' && data.length >= 24) return { width: data.readUInt32BE(16), height: data.readUInt32BE(20) };
  if (mimeType === 'image/gif' && data.length >= 10) return { width: data.readUInt16LE(6), height: data.readUInt16LE(8) };
  if (mimeType === 'image/jpeg') {
    let offset = 2;
    while (offset + 4 <= data.length) {
      if (data[offset] !== 0xff) { offset++; continue; }
      const marker = data[offset + 1];
      offset += 2;
      if (marker === 0xd8 || marker === 0xd9 || marker === 0x01 || (marker >= 0xd0 && marker <= 0xd7)) continue;
      if (offset + 2 > data.length) break;
      const length = data.readUInt16BE(offset);
      if (length < 2 || offset + length > data.length) break;
      if ((marker >= 0xc0 && marker <= 0xc3) || (marker >= 0xc5 && marker <= 0xc7) || (marker >= 0xc9 && marker <= 0xcb) || (marker >= 0xcd && marker <= 0xcf)) {
        if (length >= 7) return { height: data.readUInt16BE(offset + 3), width: data.readUInt16BE(offset + 5) };
        break;
      }
      offset += length;
    }
  }
  if (mimeType === 'image/webp' && data.length >= 30) {
    const kind = data.subarray(12, 16).toString('ascii');
    if (kind === 'VP8X') {
      return {
        width: 1 + data.readUIntLE(24, 3),
        height: 1 + data.readUIntLE(27, 3),
      };
    }
    if (kind === 'VP8L' && data.length >= 25 && data[20] === 0x2f) {
      const b0 = data[21], b1 = data[22], b2 = data[23], b3 = data[24];
      return { width: 1 + b0 + ((b1 & 0x3f) << 8), height: 1 + (b1 >> 6) + (b2 << 2) + ((b3 & 0x0f) << 10) };
    }
    if (kind === 'VP8 ' && data.length >= 30 && data[23] === 0x9d && data[24] === 0x01 && data[25] === 0x2a) {
      return { width: data.readUInt16LE(26) & 0x3fff, height: data.readUInt16LE(28) & 0x3fff };
    }
  }
  return undefined;
}

export class AssetStore {
  constructor(private root: string, private db: Db) {
    fs.mkdirSync(root, { recursive: true, mode: 0o700 });
  }

  put(agentId: string, data: Buffer, declaredMime?: string): StoredAsset {
    if (data.length > MAX_ASSET_BYTES) throw new AssetTooLargeError(`image exceeds ${MAX_ASSET_BYTES} byte limit`);
    const mimeType = sniffMime(data);
    if (!mimeType) throw new UnsupportedAssetError('only PNG, JPEG, GIF, and WebP images are supported');
    const dimensions = imageDimensions(data, mimeType);
    if (!dimensions || dimensions.width < 1 || dimensions.height < 1) throw new UnsupportedAssetError('image header is malformed or incomplete');
    if (dimensions.width > MAX_IMAGE_DIMENSION || dimensions.height > MAX_IMAGE_DIMENSION || dimensions.width * dimensions.height > MAX_IMAGE_PIXELS) {
      throw new UnsupportedAssetError(`image dimensions exceed the ${MAX_IMAGE_PIXELS} pixel safety limit`);
    }
    const normalizedDeclared = declaredMime?.split(';', 1)[0].trim().toLowerCase();
    if (normalizedDeclared && normalizedDeclared !== 'application/octet-stream' && normalizedDeclared !== mimeType) {
      throw new UnsupportedAssetError(`content type ${normalizedDeclared} does not match ${mimeType} bytes`);
    }
    const assetId = crypto.createHash('sha256').update(data).digest('hex');
    const dir = path.join(this.root, assetId.slice(0, 2));
    const file = path.join(dir, assetId);
    fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
    try {
      fs.writeFileSync(file, data, { flag: 'wx', mode: 0o600 });
    } catch (error: any) {
      if (error?.code !== 'EEXIST') throw error;
    }
    this.db.putAsset(agentId, { id: assetId, mimeType, size: data.length });
    return { assetId, mimeType, size: data.length };
  }

  get(agentId: string, assetId: string): StoredAsset & { data: Buffer } {
    if (!/^[a-f0-9]{64}$/.test(assetId)) throw new Error('asset not found');
    const meta = this.db.getAgentAsset(agentId, assetId);
    if (!meta) throw new Error('asset not found');
    const data = fs.readFileSync(path.join(this.root, assetId.slice(0, 2), assetId));
    if (data.length !== meta.size) throw new Error('asset data is corrupt');
    return { assetId, mimeType: meta.mimeType, size: meta.size, data };
  }
}
