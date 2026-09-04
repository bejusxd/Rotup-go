export type Timestamp = string | null;
export interface ApiError {
  code: string;
  class: string;
  message: string;
  userMessage: string;
}
export interface Engine {
  name: string;
  version: string;
  available: boolean;
  detail: string;
}
export interface System {
  name: string;
  version: string;
  platform: string;
  devMode: boolean;
  csrfToken: string;
  transferEngine: Engine;
  backupEngine: Engine;
  minimumRepositoryPasswordLength: number;
}
export interface SourceInput {
  name: string;
  host: string;
  shareName: string;
  subdirectory: string;
  username: string;
  stagingRelativePath: string;
  enabled: boolean;
  policy: string;
  includeRules: string[];
  excludeRules: string[];
}
export interface Source extends SourceInput {
  id: string;
  status: string;
  stateVersion: number;
  hasCredential: boolean;
  busy: boolean;
  lastPublishedAt: Timestamp;
  lastCollectionRunId: string;
}
export interface Run {
  id: string;
  kind: string;
  trigger: string;
  sourceId: string;
  status: string;
  terminal: boolean;
  repositoryId: string;
  diskId: string;
  summary: string;
  error: ApiError | null;
  requestedAt: Timestamp;
  startedAt: Timestamp;
  finishedAt: Timestamp;
  durationMs: number;
}
export interface Disk {
  id: string;
  name: string;
  registered: boolean;
  enabled: boolean;
  filesystemUuid: string;
  filesystemType: string;
  sizeBytes: number;
  status: string;
  errorCode: string;
  supported: boolean;
  ambiguous: boolean;
  busy: boolean;
  observed: boolean;
  lastConnectedAt: Timestamp;
  devicePath?: string;
  mountPoint?: string;
  externalMounts?: string[];
}
export interface DisksResponse {
  disks: Disk[];
  generatedAt: Timestamp;
  platformAvailable: boolean;
  platformReason: string;
  enumerationError: string;
}
export interface Repository {
  id: string;
  diskId: string;
  diskName: string;
  filesystemUuid: string;
  relativePath: string;
  resticId: string;
  health: string;
  reason: string;
  established: boolean;
  connected: boolean;
  busy: boolean;
  hasPassword: boolean;
  lastSnapshotId: string;
  lastSuccessfulBackupAt: Timestamp;
  establishedAt: Timestamp;
}
export interface Revision {
  id: string;
  sourceId: string;
  collectionRunId: string;
  relativePath: string;
  createdAt: Timestamp;
  itemCount: number;
  byteCount: number;
  reconciliationCompleted: boolean;
}
export interface Collection {
  stateVersion: number;
  publishedAt: Timestamp;
  bytesScanned: number;
  bytesCopied: number;
  filesScanned: number;
  filesCopied: number;
  filesDeletedOrArchived: number;
  engineErrors: number;
  warnings: string[];
}
export interface Backup {
  repositoryId: string;
  diskId: string;
  semantics: string;
  stage: string;
  durability: string;
  selectedSourceIds: string[];
  candidateSnapshotId: string;
  verifiedSnapshotId: string;
  resticStartedAt: Timestamp;
  syncedAt: Timestamp;
  unmountVerifiedAt: Timestamp;
  durableSuccessAt: Timestamp;
  bytesProcessed: number;
  bytesAdded: number;
  filesProcessed: number;
  engineErrors: number;
  readSet: {
    sourceId: string;
    collectionRunId: string;
    stateVersion: number;
    relativePath: string;
    publishedAt: Timestamp;
    capturedAt: Timestamp;
  }[];
  revisionBatches: {
    id: string;
    sourceId: string;
    relativePath: string;
    createdAt: Timestamp;
    capturedAt: Timestamp;
  }[];
}
export interface SourceDetail {
  source: Source;
  runs: Run[];
  revisionBatches: Revision[];
  historyLimit: number;
}
export interface RepositoryDetail {
  repository: Repository;
  sources: { source: Source; eligible: boolean; reason: string }[];
  history: { run: Run; backup: Backup }[];
  historyLimit: number;
  lastVerification: Run | null;
}
export interface RunDetail {
  run: Run;
  source: Source | null;
  collection: Collection | null;
  revisionBatches: Revision[];
  backup: Backup | null;
}
export interface Dashboard {
  applicationStatus: string;
  databaseStatus: string;
  sources: Source[];
  storage: DisksResponse;
  repositories: Repository[];
  recentRuns: Run[];
  recentRunLimit: number;
  lastSuccessfulBackupAt: Timestamp;
}
export interface DebugStatus {
  platform: string;
  transferEngine: Engine;
  backupEngine: Engine;
  status: Dashboard;
}
