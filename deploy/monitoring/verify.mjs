import { execFileSync } from 'node:child_process'

const variables = JSON.parse(execFileSync('railway', [
  'variable', 'list', '--project', 'adc6b001-6413-4bd3-84cf-6c6ba4bde15c',
  '--environment', 'cde449e7-b30d-4cb1-95b4-9223bceab440',
  '--service', 'cf911b58-f836-44ce-98b1-304ec9fd1be6', '--json',
], { encoding: 'utf8', timeout: 30000, env: {
  ...process.env, RAILWAY_CALLER: 'skill:use-railway@1.4.0',
  RAILWAY_AGENT_SESSION: 'railway-skill-20260907-observability',
} }))
const password = variables.GF_SECURITY_ADMIN_PASSWORD
if (!password) throw new Error('Grafana credential unavailable; retrieve it through Railway')
const base = 'https://grafana-staging-6599.up.railway.app'
const headers = { Authorization: `Basic ${Buffer.from(`admin:${password}`).toString('base64')}` }
async function get(path, authenticated = true) {
  const response = await fetch(`${base}${path}`, { headers: authenticated ? headers : {}, signal: AbortSignal.timeout(20000) })
  if (!response.ok) throw new Error(`Grafana returned ${response.status} for ${path}`)
  return response.json()
}
console.log('Grafana health:', await get('/api/health', false))
console.log('Datasource:', await get('/api/datasources/uid/vartalaap-prometheus/health'))
const dashboard = await get('/api/dashboards/uid/vartalaap-call-lab')
console.log('Dashboard:', dashboard.dashboard.title, 'panels:', dashboard.dashboard.panels.length)
const anonymous = await fetch(`${base}/api/dashboards/uid/vartalaap-call-lab`, { signal: AbortSignal.timeout(20000) })
if (anonymous.status !== 401) throw new Error('Dashboard must require authentication')
const prefix = '/api/datasources/proxy/uid/vartalaap-prometheus/api/v1/'
const query = async expression => (await get(`${prefix}query?query=${encodeURIComponent(expression)}`)).data.result
for (const panel of dashboard.dashboard.panels) {
  for (const target of panel.targets ?? []) {
    await query(target.expr.replaceAll('$__rate_interval', '5m').replaceAll('$__range', '1h'))
  }
}
console.log('All panel queries parsed successfully; anonymous dashboard access denied')
const up = await query('up{job="vartalaap-api"}')
console.log('API scrape:', up)
if (up.length !== 1 || up[0].value[1] !== '1') throw new Error('API scrape is not healthy')
for (const expression of ['vartalaap_media_rtt_seconds_count', 'vartalaap_media_packet_loss_percent_count', 'vartalaap_call_attempts_total']) {
  const samples = await query(expression)
  console.log(expression, samples)
  if (process.argv.includes('--require-media') && expression.endsWith('_count') && !samples.some(sample => Number(sample.value[1]) > 0)) {
    throw new Error(`No browser media observations collected for ${expression}`)
  }
}
const rules = await get(`${prefix}rules`)
if (process.argv.includes('--call-summary')) {
  for (const [name, expression] of Object.entries({
    peakParticipantsLast5m: 'max_over_time(vartalaap_active_peers[5m])',
    firstMediaP95SecondsLast5m: 'histogram_quantile(0.95, sum by (le) (rate(vartalaap_time_to_first_media_seconds_bucket[5m])))',
    rttP99SecondsLast5m: 'histogram_quantile(0.99, sum by (le) (rate(vartalaap_media_rtt_seconds_bucket[5m])))',
    packetLossP95PercentLast5m: 'histogram_quantile(0.95, sum by (le) (rate(vartalaap_media_packet_loss_percent_bucket[5m])))',
    jitterP95SecondsLast5m: 'histogram_quantile(0.95, sum by (le) (rate(vartalaap_media_jitter_seconds_bucket[5m])))',
    repairsLast5m: 'sum by (stage, outcome) (increase(vartalaap_sfu_repairs_total[5m]))',
  })) console.log(name, await query(expression))
}
console.log('Alert rules:', rules.data.groups.flatMap(group => group.rules.map(rule => ({ name: rule.name, health: rule.health, state: rule.state }))))
for (const group of rules.data.groups) {
  if (group.rules.some(rule => rule.health !== 'ok')) throw new Error('An alert rule is unhealthy')
}
