/**
 * Trajectory archive browser (fork feature, see railway/README.md).
 * Reads the archive bucket through the backend; nothing is stored in the DB.
 */

import { apiClient } from '../client'
import type { PaginatedResponse } from '@/types'

export interface TrajectoryDay {
  day: string
  objects: number
  records: number
  bytes: number
}

export interface TrajectorySummary {
  objects: number
  records: number
  bytes: number
  days: TrajectoryDay[] | null
  spool_files: number
  dropped: number
  queue_length: number
  bucket: string
  prefix: string
  generated_at: string
}

export interface TrajectoryEntry {
  ts: string
  request_id?: string
  client_request_id?: string
  session_id?: string
  method: string
  path: string
  status: number
  latency_ms: number
  user_id?: number
  api_key_id?: number
  group_id?: number
  account_id?: number
  model?: string
  upstream_model?: string
  stream: boolean
  response_content_type?: string
  line: number
  request_bytes: number
  response_bytes: number
  key: string
}

export interface TrajectoryGroup {
  key: string
  count: number
  errors: number
  avg_latency_ms: number
  request_bytes: number
  response_bytes: number
  first: string
  last: string
  models: string[] | null
  user_id?: number
}

export type TrajectoryGroupBy = '' | 'session' | 'model' | 'user' | 'api_key' | 'account' | 'group' | 'path' | 'status'

export interface TrajectoryQuery {
  page?: number
  page_size?: number
  start_time?: string
  end_time?: string
  request_id?: string
  session_id?: string
  model?: string
  path?: string
  user_id?: string
  api_key_id?: string
  account_id?: string
  group_id?: string
  status?: string
  group_by?: TrajectoryGroupBy
  order?: 'asc' | 'desc'
}

/** Full archived record: entry metadata plus headers and bodies. */
export interface TrajectoryRecord extends Omit<TrajectoryEntry, 'line' | 'key' | 'request_bytes' | 'response_bytes'> {
  request_headers?: Record<string, string>
  request_body: unknown
  response_body: unknown
}

export async function summary(): Promise<{ enabled: boolean; summary?: TrajectorySummary }> {
  const { data } = await apiClient.get('/admin/trajectories/summary')
  return data
}

export async function records(
  params: TrajectoryQuery
): Promise<PaginatedResponse<TrajectoryEntry | TrajectoryGroup>> {
  const { data } = await apiClient.get('/admin/trajectories/records', { params })
  return data
}

export async function record(key: string, line: number): Promise<TrajectoryRecord> {
  const { data } = await apiClient.get('/admin/trajectories/record', { params: { key, line } })
  return data
}

export const trajectoriesAPI = { summary, records, record }
export default trajectoriesAPI
