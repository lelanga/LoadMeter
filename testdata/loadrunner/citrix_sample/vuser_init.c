vuser_init()
{
	ctrx_set_connect_opt(APPLICATION, "#Notepad");
	ctrx_set_connect_opt(NETWORK_PROTOCOL, "HTTPonTCP/IP");
	ctrx_connect_server("ica.example.test", "user", "pass", "DOMAIN", CTRX_LAST);
	return 0;
}
