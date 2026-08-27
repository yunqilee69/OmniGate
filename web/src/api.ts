const token = localStorage.getItem('admin_token') ?? ''

export async function api<T = any>(method: string, path: string, body?: any): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { 'X-Admin-Token': token } : {}),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`
    try {
      const j = await res.json()
      msg = j.error?.message ?? JSON.stringify(j)
    } catch {
      /* 非 JSON 错误体，保留状态码信息 */
    }
    throw new Error(msg)
  }
  return res.json()
}

export const setToken = (t: string) => localStorage.setItem('admin_token', t)
export const getToken = () => token
