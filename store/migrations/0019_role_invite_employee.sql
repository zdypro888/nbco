-- Keep existing deployments aligned with the renamed employee invite tool.
UPDATE roles
SET prompt = replace(prompt, 'generate_key 生成真人员工入职 Key', 'invite_employee 生成真人员工一次性邀请')
WHERE name = 'HR招聘';
