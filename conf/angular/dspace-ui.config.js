// pm2 process definition for the DSpace UI SSR server, used by the
// "production" target in Dockerfile-angular. PM2_INSTANCES sets how many
// cluster workers run; "max" means one per CPU core.
module.exports = {
  apps: [
    {
      name: 'dspace-ui',
      cwd: '/app',
      script: 'dist/server/main.js',
      instances: process.env.PM2_INSTANCES || 'max',
      exec_mode: 'cluster',
    },
  ],
};
