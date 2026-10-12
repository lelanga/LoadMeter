vuser_end()
{
	ctrx_disconnect_server("ica.example.test", CTRX_LAST);
	return 0;
}
