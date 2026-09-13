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

/**
 * 个人主页（阶段6）。**公开接口，不带 authRequired** —— 看别人的主页不需要登录。
 *
 * 它是全项目唯一的**跨模块聚合**读接口：一次返回账号信息 + 三个模块的计数
 * （videos 的作品数与总获赞、social 的粉丝数与关注数）。后端为此单开了一个
 * internal/profile 包，而**不是**塞进 accountHandler —— 因为 account 包
 * 不能 import video/social（video 已经 import 了 account，反向依赖会成环）。
 *
 * account 的形状和 findByID 一致（同一个 FindByIDResponse），
 * 所以可以直接喂给 AccountInfo，也可以直接塞进 AuthorCard。
 *
 * 四个计数是**各自独立查出来的**，任何一个查询失败都降级成 0 并 log，
 * 不影响其余字段 —— 所以"粉丝 0"既可能是真 0，也可能是那次 COUNT 失败了。
 * 前端不做区分（后端也没在响应里留痕）。
 */
export interface ProfileResult {
  account: AccountInfo
  video_count: number
  total_likes: number
  follower_count: number
  vlogger_count: number
}

export function getProfile(accountID: number) {
  return postJson<ProfileResult>('/account/getProfile', { account_id: accountID })
}
