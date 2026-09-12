// 账号模块的 API：每个函数对应后端一个接口

import { postForm, postJson } from './client'

export interface LoginResult {
  token: string
  refresh_token: string
  account_id: number
  username: string
}

export interface AccountInfo {
  id: number
  username: string
  avatar_url?: string
  bio?: string
}

export function register(username: string, password: string) {
  return postJson<{ message: string }>('/account/register', { username, password })
}

export function login(username: string, password: string) {
  return postJson<LoginResult>('/account/login', { username, password })
}

export function logout() {
  return postJson<{ message: string }>('/account/logout', {}, { authRequired: true })
}

export function rename(newUsername: string) {
  return postJson<{ token: string }>('/account/rename', { new_username: newUsername }, { authRequired: true })
}

export function findByID(id: number) {
  return postJson<AccountInfo>('/account/findByID', { id })
}

export function findByUsername(username: string) {
  return postJson<{ id: number; username: string }>('/account/findByUsername', { username })
}

export function updateProfile(payload: { bio?: string; avatar_url?: string }) {
  return postJson<{ message: string }>('/account/updateProfile', payload, { authRequired: true })
}

export function uploadAvatar(file: File) {
  const fd = new FormData()
  fd.append('file', file)
  return postForm<{ avatar_url: string }>('/account/uploadAvatar', fd, { authRequired: true })
}
