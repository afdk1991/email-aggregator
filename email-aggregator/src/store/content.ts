/**
 * 内容存储（对象存储，内容寻址）。
 * 邮件正文/附件以 contentHash 为键只存一份（去重），落 MinIO / S3 / 托管对象存储。
 * PoC 以 InMemoryContentStore 实现；ObjectContentStore 为真实集成 stub。
 */

import { createHash } from "node:crypto";

export interface StoredContent {
  objectKey: string;
  contentHash: string;
  size: number;
}

export interface ContentStore {
  /** 写入内容，返回对象键与哈希（相同内容自动去重） */
  put(content: Buffer): Promise<StoredContent>;
  get(objectKey: string): Promise<Buffer>;
}

export class InMemoryContentStore implements ContentStore {
  private objects = new Map<string, Buffer>(); // objectKey -> bytes

  async put(content: Buffer): Promise<StoredContent> {
    const hash = createHash("sha256").update(content).digest("hex");
    const objectKey = `mail/${hash}`;
    if (!this.objects.has(objectKey)) this.objects.set(objectKey, content);
    return { objectKey, contentHash: hash, size: content.length };
  }

  async get(objectKey: string): Promise<Buffer> {
    const b = this.objects.get(objectKey);
    if (!b) throw new Error(`content not found: ${objectKey}`);
    return b;
  }
}

/**
 * @deprecated STUB — 未实现。仅作接口文档保留。
 * 真实对象存储适配器落地时改为（S3/MinIO 兼容）：
 *   const key = `mail/${sha256}`;
 *   if (!(await exists(key))) await s3.putObject(bucket, key, content);
 * Go 版真实实现见 integration/integration.go 中的 ObjectContentStore。
 */
export class ObjectContentStore implements ContentStore {
  private endpoint: string;
  private bucket: string;
  constructor(endpoint: string, bucket: string) {
    this.endpoint = endpoint;
    this.bucket = bucket;
    console.warn(`[ObjectContentStore] stub: ${endpoint}/${bucket}`);
  }
  async put(_content: Buffer): Promise<StoredContent> {
    throw new Error("ObjectContentStore.put not implemented");
  }
  async get(_key: string): Promise<Buffer> {
    throw new Error("ObjectContentStore.get not implemented");
  }
}
