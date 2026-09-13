CREATE USER IF NOT EXISTS 'ocservia_app'@'%' IDENTIFIED BY 'development-runtime-only';
GRANT ALL PRIVILEGES ON ocservia.* TO 'ocservia_owner'@'%' WITH GRANT OPTION;
