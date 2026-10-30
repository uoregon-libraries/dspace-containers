// pm2 process definitions for the DSpace UI, used by the "production" target
// in Dockerfile-angular. PM2_INSTANCES sets how many cluster workers the SSR
// server runs; "max" means one per CPU core. PM2_MAX_MEMORY_RESTART, if set
// (e.g., "1G"), caps each worker's memory. The watchdog restarts the
// container when SSR gets stuck (see watchdog.js).
const maxMemory = process.env.PM2_MAX_MEMORY_RESTART;

module.exports = {
  apps: [
    {
      name: 'dspace-ui',
      cwd: '/app',
      script: 'dist/server/main.js',
      instances: process.env.PM2_INSTANCES || 'max',
      exec_mode: 'cluster',
      // pm2 checks every 30s and replaces any worker over the limit, starting
      // the new worker before stopping the old one, so the site stays up
      ...(maxMemory && { max_memory_restart: maxMemory }),
    },
    {
      name: 'watchdog',
      cwd: '/app',
      script: 'watchdog.js',
    },
  ],
};
