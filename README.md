# 1. Без токена — 401
curl -s http://localhost:8080/api/hello
# {"code":401,"message":"missing token","request_id":"..."}

# 2. Логин
curl -s -X POST http://localhost:8080/v1/login \
-H "Content-Type: application/json" \
-d '{"subject":"admin","password":"admin"}'