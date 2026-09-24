INSERT INTO client_services(id,display_name) VALUES
  ('retail-bot','Retail Telegram adapter'),('reseller-bot','Reseller Telegram adapter')
ON CONFLICT(id) DO NOTHING;
INSERT INTO panels(id,base_url) VALUES
  ('panel-retail-finland','https://configure-panel.invalid'),
  ('panel-retail-germany','https://configure-panel.invalid'),
  ('panel-reseller-turk1','https://configure-panel.invalid')
ON CONFLICT(id) DO NOTHING;
INSERT INTO deployments(id,client_service_id,channel,legacy_source_instance,default_panel_id) VALUES
  ('retail-finland','retail-bot','retail','retail-finland','panel-retail-finland'),
  ('retail-germany','retail-bot','retail','retail-germany','panel-retail-germany'),
  ('reseller-turk1','reseller-bot','reseller','reseller-turk1','panel-reseller-turk1')
ON CONFLICT(id) DO NOTHING;
