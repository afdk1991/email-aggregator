/**
 * KMS 凭据保险库 —— 信封加密（Envelope Encryption，ADR-004）。
 *
 * 设计要点：
 *  - 每份凭据用一次性随机 DEK（数据加密密钥）以 AES-256-GCM 加密。
 *  - DEK 再用主密钥 KEK（由 KMS 管理）"包裹"（wrap）后存储。
 *  - 落库只存：ciphertext + iv + tag + wrappedDek + keyVersion。
 *  - 平台从不持有明文主密钥；真实环境 KEK 由云 KMS（如 KMS / Vault Transit）托管，
 *    本 PoC 以 InMemoryKMS 模拟（KEK 仅存内存、进程重启即失）。
 *  - 支持密钥轮换：re-seal 时把 DEK 重新包裹到当前 keyVersion。
 *
 * 安全约束：
 *  - 解密后的明文凭据仅存于短期内存，绝不落盘、不进日志。
 *  - DEK 一次性，即使单个 DEK 泄露也只影响一份凭据。
 */

import { createCipheriv, createDecipheriv, randomBytes } from "node:crypto";

const ALGO = "aes-256-gcm";
const DEK_LEN = 32; // 256-bit
const IV_LEN = 12; // GCM 推荐 96-bit

/** KMS 抽象：真实环境替换为云厂商 SDK（加密/解密 DEK） */
export interface Kms {
  currentKeyVersion(): number;
  /** 用主密钥包裹 DEK */
  wrap(dek: Buffer): { wrappedDek: Buffer; keyVersion: number };
  /** 解包 DEK */
  unwrap(wrappedDek: Buffer, keyVersion: number): Buffer;
}

/** 进程内 KMS 占位（演示用，KEK 仅内存） */
export class InMemoryKms implements Kms {
  private kek: Buffer;
  private version: number;

  constructor(kek?: Buffer, version = 1) {
    this.kek = kek ?? randomBytes(32);
    this.version = version;
  }

  currentKeyVersion(): number {
    return this.version;
  }

  wrap(dek: Buffer): { wrappedDek: Buffer; keyVersion: number } {
    const iv = randomBytes(IV_LEN);
    const c = createCipheriv(ALGO, this.kek, iv);
    const enc = Buffer.concat([c.update(dek), c.final()]);
    const tag = c.getAuthTag();
    // 简单拼接：wrappedDek = iv(12) + tag(16) + enc
    return { wrappedDek: Buffer.concat([iv, tag, enc]), keyVersion: this.version };
  }

  unwrap(wrappedDek: Buffer, keyVersion: number): Buffer {
    if (keyVersion !== this.version) {
      throw new Error(`kek version mismatch: have ${this.version}, want ${keyVersion}`);
    }
    const iv = wrappedDek.subarray(0, IV_LEN);
    const tag = wrappedDek.subarray(IV_LEN, IV_LEN + 16);
    const enc = wrappedDek.subarray(IV_LEN + 16);
    const c = createDecipheriv(ALGO, this.kek, iv);
    c.setAuthTag(tag);
    return Buffer.concat([c.update(enc), c.final()]);
  }
}

export interface SealedCredential {
  ciphertext: string; // base64
  iv: string; // base64
  tag: string; // base64
  wrappedDek: string; // base64
  keyVersion: number;
  /** 加密时标记，便于审计 */
  sealedAt: number;
}

const b64 = (b: Buffer) => b.toString("base64");
const fromB64 = (s: string) => Buffer.from(s, "base64");

export class CredentialVault {
  private kms: Kms;
  constructor(kms: Kms) {
    this.kms = kms;
  }

  /** 信封加密：生成 DEK → 加密明文 → 用 KEK 包裹 DEK */
  async seal(plaintext: string): Promise<SealedCredential> {
    const dek = randomBytes(DEK_LEN);
    const iv = randomBytes(IV_LEN);
    const c = createCipheriv(ALGO, dek, iv);
    const enc = Buffer.concat([c.update(Buffer.from(plaintext, "utf8")), c.final()]);
    const tag = c.getAuthTag();
    const { wrappedDek, keyVersion } = this.kms.wrap(dek);
    return {
      ciphertext: b64(enc),
      iv: b64(iv),
      tag: b64(tag),
      wrappedDek: b64(wrappedDek),
      keyVersion,
      sealedAt: Date.now(),
    };
  }

  /** 解封：解包 DEK → AES-GCM 解密 */
  async unseal(sealed: SealedCredential): Promise<string> {
    const dek = this.kms.unwrap(fromB64(sealed.wrappedDek), sealed.keyVersion);
    const c = createDecipheriv(ALGO, dek, fromB64(sealed.iv));
    c.setAuthTag(fromB64(sealed.tag));
    const dec = Buffer.concat([c.update(fromB64(sealed.ciphertext)), c.final()]);
    return dec.toString("utf8");
  }

  /** 密钥轮换：解封后用当前 keyVersion 重新密封（DEK 亦换新） */
  async rotate(sealed: SealedCredential): Promise<SealedCredential> {
    const plaintext = await this.unseal(sealed);
    return this.seal(plaintext);
  }
}
