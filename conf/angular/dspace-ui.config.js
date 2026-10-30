// pm2 process definitions for the DSpace UI, used by the "production" target
// in Dockerfile-angular. PM2_INSTANCES sets how many cluster workers the SSR
// server runs; "max" means one per CPU core. The watchdog restarts the
// container when SSR gets stuck (see watchdog.js).
module.exports = {
  apps: [
    {
      name: 'dspace-ui',
      cwd: '/app',
      script: 'dist/server/main.js',
      instances: process.env.PM2_INSTANCES || 'max',
      exec_mode: 'cluster',
    },
    {
      name: 'watchdog',
      cwd: '/app',
      script: 'watchdog.js',
    },
  ],
};
